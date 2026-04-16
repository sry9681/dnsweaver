#!/usr/bin/env bash
#
# Contained end-to-end test of docker_mode={swarm,standalone,both} using
# Docker-in-Docker. Covers:
#
#   Phase 1 - startup guards on a non-Swarm daemon
#   Phase 2 - create/delete lifecycle against a real webhook sink for
#             every mode, plus failing-container edge case
#   Phase 3 - non-manager topology: a second dind joins as a Swarm worker
#             and we verify the manager-only guards reject mode=swarm/both
#             while mode=standalone still works (with task filter)
#
# Design
#   - ONE dind-manager runs phases 1+2. A second dind-worker is added for
#     phase 3. Both dinds share a user-defined outer bridge network so the
#     worker can reach the manager by container name.
#   - A tiny Python HTTP sink runs inside dind-manager as the webhook target.
#     It satisfies /ping, /list, /create, /delete and appends every request
#     to a log file. Assertions grep that file.
#   - dnsweaver is built ONCE inside dind-manager, then saved and loaded into
#     dind-worker to avoid a second ~25s build.
#   - Hostnames are unique per test case so assertions can't cross-contaminate
#     and the sink log reads like a flight recorder.
#
# Safety
#   - All work happens in privileged docker:dind containers and a dedicated
#     bridge network on the host. Your host daemon is untouched beyond
#     creating/removing those artifacts (cleanup trap handles removal).
#   - DOCKER_HOST is scoped per-command via the `dm()` / `dw()` wrappers and
#     is never exported.
#
# Usage
#   scripts/test-mode-both-dind.sh              # full run, all three phases
#   scripts/test-mode-both-dind.sh --skip-phase3  # skip the worker test
#   scripts/test-mode-both-dind.sh --keep        # leave everything running
#
set -Eeuo pipefail

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------
DIND_IMAGE="docker:27-dind"
NET_NAME="dnsweaver-testnet"         # outer bridge (for dind <-> dind)
INNER_NET="dnsweaver-inner"          # inside dind-manager (for sink + dnsweaver DNS)
MANAGER_NAME="dnsweaver-dind-manager"
WORKER_NAME="dnsweaver-dind-worker"
MANAGER_PORT="23751"
WORKER_PORT="23752"
SINK_NAME="dnsweaver-sink"
SINK_PORT="18080"               # host-visible port on the sink (debug only)
DNSWEAVER_TAG="dnsweaver:mode-both-dind"
SINK_LOG="/data/log"
DOMAIN="modebothtest.invalid"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

SKIP_PHASE3=0
KEEP=0
for arg in "$@"; do
    case "$arg" in
        --skip-phase3) SKIP_PHASE3=1 ;;
        --keep) KEEP=1 ;;
        -h|--help) sed -n '2,30p' "${BASH_SOURCE[0]}"; exit 0 ;;
        *) echo "unknown flag: $arg" >&2; exit 2 ;;
    esac
done

# ---------------------------------------------------------------------------
# Logging helpers
# ---------------------------------------------------------------------------
phase() { printf '\n\033[1;35m═══ %s ═══\033[0m\n' "$*"; }
log()   { printf '\n\033[1;34m[test]\033[0m %s\n' "$*"; }
info()  { printf '       %s\n' "$*"; }
pass()  { printf '\033[1;32m  ✓\033[0m %s\n' "$*"; ((PASSED++)) || true; }
fail()  { printf '\033[1;31m  ✗\033[0m %s\n' "$*"; ((FAILED++)) || true; }

PASSED=0
FAILED=0

# ---------------------------------------------------------------------------
# Cleanup — runs on any exit
# ---------------------------------------------------------------------------
cleanup() {
    local ec=$?
    if [[ $FAILED -gt 0 ]]; then
        echo >&2
        log "dumping diagnostic logs (tests failed)"
        for dnsweaver_host in "$MANAGER_PORT" "$WORKER_PORT"; do
            echo "--- dnsweaver logs on :${dnsweaver_host} ---" >&2
            DOCKER_HOST="tcp://localhost:${dnsweaver_host}" docker logs dnsweaver 2>&1 | tail -80 >&2 || true
        done
        echo "--- sink container logs (stderr/stdout) ---" >&2
        DOCKER_HOST="tcp://localhost:${MANAGER_PORT}" docker logs "$SINK_NAME" 2>&1 | tail -40 >&2 || true
        echo "--- sink request log ($SINK_LOG) ---" >&2
        DOCKER_HOST="tcp://localhost:${MANAGER_PORT}" docker exec "$SINK_NAME" cat "$SINK_LOG" 2>&1 >&2 || true
    fi
    if [[ $KEEP -eq 1 ]]; then
        log "--keep set; leaving containers + network for inspection"
        log "manager:  export DOCKER_HOST=tcp://localhost:${MANAGER_PORT}"
        log "worker:   export DOCKER_HOST=tcp://localhost:${WORKER_PORT}"
        log "sink UI:  http://localhost:${SINK_PORT}/list"
        return
    fi
    log "cleanup"
    docker rm -f "$MANAGER_NAME" "$WORKER_NAME" >/dev/null 2>&1 || true
    docker network rm "$NET_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Scoped docker wrappers — never leak DOCKER_HOST globally
# ---------------------------------------------------------------------------
dm() { DOCKER_HOST="tcp://localhost:${MANAGER_PORT}" docker "$@"; }
dw() { DOCKER_HOST="tcp://localhost:${WORKER_PORT}"  docker "$@"; }

# Wait until a predicate returns 0, up to N seconds.
wait_for() {
    local desc="$1"; shift
    local timeout="${1:-30}"; shift || true
    local start; start=$(date +%s)
    while ! "$@" >/dev/null 2>&1; do
        if (( $(date +%s) - start > timeout )); then
            fail "timeout waiting for: $desc"
            return 1
        fi
        sleep 1
    done
    info "ready: $desc"
}

# ---------------------------------------------------------------------------
# Outer network + dind-manager
# ---------------------------------------------------------------------------
start_manager() {
    log "create outer bridge network $NET_NAME"
    docker network inspect "$NET_NAME" >/dev/null 2>&1 || \
        docker network create "$NET_NAME" >/dev/null

    log "start $MANAGER_NAME (port $MANAGER_PORT)"
    docker rm -f "$MANAGER_NAME" >/dev/null 2>&1 || true
    docker run -d --privileged --name "$MANAGER_NAME" \
        --network "$NET_NAME" \
        -p "${MANAGER_PORT}:2375" \
        -e DOCKER_TLS_CERTDIR= \
        "$DIND_IMAGE" --host=tcp://0.0.0.0:2375 >/dev/null
    wait_for "manager daemon responsive" 30 dm info

    # User-defined bridge inside dind so sink + dnsweaver can resolve each
    # other by container name. Docker's default bridge does not provide
    # container-name DNS.
    dm network inspect "$INNER_NET" >/dev/null 2>&1 || \
        dm network create "$INNER_NET" >/dev/null
}

# ---------------------------------------------------------------------------
# Webhook sink: Python http.server that logs every request and returns
# plausible responses for dnsweaver's webhook protocol.
# ---------------------------------------------------------------------------
start_sink() {
    log "start HTTP sink inside dind-manager"

    # Write the sink program to a tempfile on the outer host, then stream
    # it into the dind container's filesystem via `docker exec` — avoids
    # the bind-mount-source-autocreated-as-directory gotcha.
    local sinkpy
    sinkpy=$(mktemp)
    # shellcheck disable=SC2064
    trap "rm -f $sinkpy" RETURN

    cat > "$sinkpy" <<'PY'
# Stateful webhook-compatible HTTP sink. Acts as a real (in-memory) DNS
# backend so dnsweaver's ownership-based orphan-deletion logic can
# complete: /list returns actual stored records, enabling dnsweaver to
# verify it owns a record before deleting it.
import json, os, threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOG = os.environ.get("SINK_LOG", "/data/log")
os.makedirs(os.path.dirname(LOG), exist_ok=True)

_lock = threading.Lock()
_records = {}  # (hostname, type) -> {"hostname","type","value","ttl"}

class H(BaseHTTPRequestHandler):
    def _log(self, body):
        with open(LOG, "a") as f:
            f.write(f"{self.command} {self.path} {body.decode('utf-8','replace')}\n")

    def _send(self, code, payload=b"{}"):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def _body(self):
        n = int(self.headers.get("content-length", "0") or "0")
        return self.rfile.read(n) if n else b""

    def do_GET(self):
        self._log(b"")
        if self.path == "/ping":
            return self._send(200, b'{"status":"ok"}')
        if self.path == "/list":
            with _lock:
                payload = json.dumps(list(_records.values())).encode()
            return self._send(200, payload)
        return self._send(200, b"{}")

    def do_POST(self):
        body = self._body()
        self._log(body)
        if self.path == "/create":
            try:
                rec = json.loads(body or b"{}")
                key = (rec.get("hostname",""), rec.get("type",""))
                with _lock:
                    _records[key] = rec
            except Exception:
                pass
        self._send(200)

    def do_PUT(self):
        body = self._body()
        self._log(body)
        if self.path == "/update":
            try:
                rec = json.loads(body or b"{}")
                key = (rec.get("hostname",""), rec.get("type",""))
                with _lock:
                    _records[key] = rec
            except Exception:
                pass
        self._send(200)

    def do_DELETE(self):
        body = self._body()
        # Test-only reset: clears in-memory records AND truncates the log
        # so the next test phase starts from a clean slate.
        if self.path == "/_reset":
            with _lock:
                _records.clear()
            open(LOG, "w").close()
            return self._send(200, b'{"reset":true}')
        self._log(body)
        if self.path == "/delete":
            try:
                rec = json.loads(body or b"{}")
                key = (rec.get("hostname",""), rec.get("type",""))
                with _lock:
                    _records.pop(key, None)
            except Exception:
                pass
        self._send(200)

    # Suppress default access logging.
    def log_message(self, *args, **kwargs):
        return

if __name__ == "__main__":
    ThreadingHTTPServer(("", 8080), H).serve_forever()
PY

    # Stage 1: create the container with /sink.py staged via a busybox
    # init loop, so we can copy the script in BEFORE python runs.
    # We use `sh -c "mkdir /data && while [ ! -f /sink.py ]; do sleep 0.1;
    # done && exec python -u /sink.py"` as the entrypoint — python becomes
    # the container's main process (PID 1 after exec), so its stderr goes
    # to `docker logs` and `docker inspect` reflects its state.
    dm rm -f "$SINK_NAME" >/dev/null 2>&1 || true
    dm run -d --name "$SINK_NAME" \
        --network "$INNER_NET" \
        -p "${SINK_PORT}:8080" \
        -e "SINK_LOG=${SINK_LOG}" \
        python:3-alpine sh -c \
        'mkdir -p /data; echo waiting for /sink.py; until [ -f /sink.py ]; do sleep 0.1; done; echo starting python sink; exec python -u /sink.py' \
        >/dev/null

    # Outer-host tempfile → dm cp straight into the sink container. The
    # dind daemon owns $SINK_NAME; source path is resolved on the calling
    # host. Copy triggers the entrypoint loop to exec python.
    info "sink.py staged at $sinkpy ($(wc -c < "$sinkpy") bytes)"
    dm cp "$sinkpy" "${SINK_NAME}:/sink.py"
    info "sink.py inside container: $(dm exec "$SINK_NAME" wc -c /sink.py 2>&1 || echo 'stat failed')"

    # Before polling /ping, confirm the container is still running.
    local status; status=$(dm inspect -f '{{.State.Status}}' "$SINK_NAME" 2>/dev/null)
    if [[ "$status" != "running" ]]; then
        info "sink container status: $status"
        info "sink container logs:"
        dm logs "$SINK_NAME" 2>&1 | sed 's/^/       sink| /'
        fail "sink container exited before python started serving"
        return 1
    fi

    # Probe via python itself (we know python is in the image). Using
    # urllib avoids wget/nc/curl availability differences.
    local probe='import urllib.request,sys
try:
  sys.exit(0 if urllib.request.urlopen("http://127.0.0.1:8080/ping",timeout=1).status==200 else 1)
except Exception as e:
  sys.stderr.write(str(e)); sys.exit(2)'
    wait_for "sink responsive" 25 \
        dm exec "$SINK_NAME" python -c "$probe"

    info "sink logging to ${SINK_NAME}:${SINK_LOG}"
}

sink_log() { dm exec "$SINK_NAME" cat "$SINK_LOG" 2>/dev/null || true; }
sink_reset() {
    # Clears both the request log and the in-memory record store so
    # ownership state from previous modes doesn't bleed into this one.
    dm exec "$SINK_NAME" python -c \
        'import urllib.request; urllib.request.urlopen(urllib.request.Request("http://127.0.0.1:8080/_reset",method="DELETE"),timeout=2).read()' \
        >/dev/null 2>&1 || true
}

# Count sink log lines matching METHOD for records with a specific hostname
# AND record type. Matching both fields rules out:
#   - the ownership TXT record (has "hostname":"_dnsweaver.<host>")
#   - partial-name matches from other tests
#   - wrong-type bugs (e.g. dnsweaver sending DELETE for a CNAME instead of A)
# Default type is A since all test workloads configure A records.
sink_count() {
    local method="$1" host="$2" rtype="${3:-A}"
    local pat_host="\"hostname\":\"${host}\""
    local pat_type="\"type\":\"${rtype}\""
    sink_log | awk -v m="$method" -v ph="$pat_host" -v pt="$pat_type" '
        $1 == m && index($0, ph) > 0 && index($0, pt) > 0 { c++ } END { print c+0 }'
}

# Poll until sink_count >= expected or timeout. Returns 0 on success.
sink_wait_for() {
    local desc="$1" method="$2" needle="$3" expected="${4:-1}" timeout="${5:-20}"
    local start; start=$(date +%s)
    while (( $(sink_count "$method" "$needle") < expected )); do
        if (( $(date +%s) - start > timeout )); then
            fail "timeout waiting for $desc (saw $(sink_count "$method" "$needle"), want >= $expected)"
            return 1
        fi
        sleep 1
    done
    pass "$desc"
}

# Reconcile count snapshot. Each completed reconcile emits a
# "reconciliation complete" line; we count them to convert time-based
# negative assertions into event-based ones.
reconcile_mark() {
    # awk avoids grep -c's exit-1-on-zero-matches behavior which would
    # terminate the script under `set -e`.
    dm logs dnsweaver 2>&1 | awk '/reconciliation complete/{n++} END{print n+0}'
}

# Block until at least one reconcile has completed since the given mark.
# This is the linchpin of correct negative assertions: we can only claim
# "no CREATE happened" AFTER a reconcile that WOULD have fired the CREATE
# has actually run.
wait_for_reconcile_after() {
    local before="$1" timeout="${2:-25}" start; start=$(date +%s)
    while (( $(reconcile_mark) <= before )); do
        if (( $(date +%s) - start > timeout )); then
            return 1
        fi
        sleep 1
    done
    return 0
}

# Event-based absence: proves a reconcile has run since $mark AND did not
# produce the expected $method call for $host. Closes the "slow machine →
# reconcile simply hasn't fired yet → false-pass" timing hole.
sink_assert_absent_after_reconcile() {
    local desc="$1" method="$2" host="$3" mark="$4" rtype="${5:-A}"
    if ! wait_for_reconcile_after "$mark" 25; then
        fail "$desc (timed out waiting for post-deploy reconcile; cannot prove absence)"
        return 1
    fi
    # Extra debounce cushion — in case the reconcile that fired was from a
    # prior event and the workload-triggered one is still being debounced.
    sleep 3
    if ! wait_for_reconcile_after "$mark" 15; then
        fail "$desc (second reconcile never fired)"
        return 1
    fi
    local n; n=$(sink_count "$method" "$host" "$rtype")
    if (( n == 0 )); then pass "$desc"
    else fail "$desc (saw $n $method $rtype lines for $host)"; fi
}

# ---------------------------------------------------------------------------
# dnsweaver runner (manager or worker)
# ---------------------------------------------------------------------------
# start_dnsweaver <docker-helper> <mode> <expected: ready|exit>
# docker-helper is "dm" or "dw"; mode is swarm|standalone|both.
# If expected=ready, wait for "docker client initialized" and return 0.
# If expected=exit, wait for container exit and return 0 iff it exited
# nonzero (the guard fired) — used by startup-guard assertions.
start_dnsweaver() {
    local helper="$1" mode="$2" expected="$3" sink_addr="$4"
    "$helper" rm -f dnsweaver >/dev/null 2>&1 || true

    # Attach to the inner bridge network ONLY on the manager (the sink
    # lives there). On the worker we use a bogus sink addr that we expect
    # dnsweaver to never reach — startup guards fire before any HTTP call.
    local net_args=()
    if [[ "$helper" == "dm" ]]; then
        net_args=(--network "$INNER_NET")
    fi

    "$helper" run -d --name dnsweaver --user 0:0 \
        "${net_args[@]}" \
        -v /var/run/docker.sock:/var/run/docker.sock:ro \
        -e DNSWEAVER_DRY_RUN=false \
        -e DNSWEAVER_LOG_LEVEL=info \
        -e DNSWEAVER_LOG_FORMAT=text \
        -e DNSWEAVER_DOCKER_MODE="$mode" \
        -e DNSWEAVER_INSTANCE_ID="mode-${mode}-test" \
        -e DNSWEAVER_RECONCILE_INTERVAL=15s \
        -e DNSWEAVER_SOURCES="dnsweaver" \
        -e DNSWEAVER_INSTANCES="testhook" \
        -e DNSWEAVER_TESTHOOK_TYPE="webhook" \
        -e DNSWEAVER_TESTHOOK_RECORD_TYPE="A" \
        -e DNSWEAVER_TESTHOOK_TARGET="10.99.0.1" \
        -e DNSWEAVER_TESTHOOK_DOMAINS="*.${DOMAIN}" \
        -e DNSWEAVER_TESTHOOK_URL="http://${sink_addr}:8080" \
        "$DNSWEAVER_TAG" >/dev/null

    if [[ "$expected" == "exit" ]]; then
        local waited=0
        while (( waited < 20 )); do
            local status
            status=$("$helper" inspect -f '{{.State.Status}}' dnsweaver 2>/dev/null || echo missing)
            if [[ "$status" == "exited" || "$status" == "dead" ]]; then
                local code
                code=$("$helper" inspect -f '{{.State.ExitCode}}' dnsweaver 2>/dev/null || echo 0)
                if [[ "$code" != "0" ]]; then return 0; fi
                return 1
            fi
            sleep 1; waited=$((waited+1))
        done
        return 1
    fi

    # expected=ready
    local waited=0
    while (( waited < 30 )); do
        local status
        status=$("$helper" inspect -f '{{.State.Status}}' dnsweaver 2>/dev/null || echo missing)
        if [[ "$status" == "exited" || "$status" == "dead" ]]; then
            fail "dnsweaver exited before init (expected ready, mode=$mode)"
            return 1
        fi
        if "$helper" logs dnsweaver 2>&1 | grep -q 'docker client initialized'; then
            # Give one reconcile + debounce interval to settle.
            sleep 5
            return 0
        fi
        sleep 1; waited=$((waited+1))
    done
    fail "timeout waiting for dnsweaver init (mode=$mode)"
    return 1
}

stop_dnsweaver() {
    local helper="$1"
    "$helper" rm -f dnsweaver >/dev/null 2>&1 || true
}

# =============================================================================
# PHASE 1 — Startup guards on a non-Swarm daemon
# =============================================================================
phase1() {
    phase "Phase 1 — Startup guards (no Swarm)"

    # At this point the manager dind is up but `swarm init` has NOT run.
    # Expect: swarm + both reject, standalone starts.

    log "mode=swarm on non-Swarm daemon → should fail with ErrSwarmNotActive"
    if start_dnsweaver dm swarm exit "$SINK_NAME"; then
        if dm logs dnsweaver 2>&1 | grep -qi 'swarm.*not.*active\|swarm not active'; then
            pass "mode=swarm rejected with ErrSwarmNotActive"
        else
            fail "mode=swarm exited but error wasn't ErrSwarmNotActive"
        fi
    else
        fail "mode=swarm did not exit (it should have)"
    fi
    stop_dnsweaver dm

    log "mode=both on non-Swarm daemon → should fail with ErrSwarmNotActive"
    if start_dnsweaver dm both exit "$SINK_NAME"; then
        if dm logs dnsweaver 2>&1 | grep -qi 'swarm.*not.*active\|swarm not active'; then
            pass "mode=both rejected with ErrSwarmNotActive"
        else
            fail "mode=both exited but error wasn't ErrSwarmNotActive"
        fi
    else
        fail "mode=both did not exit (it should have)"
    fi
    stop_dnsweaver dm

    log "mode=standalone on non-Swarm daemon → should start successfully"
    if start_dnsweaver dm standalone ready "$SINK_NAME"; then
        pass "mode=standalone initialized on non-Swarm daemon"
    else
        fail "mode=standalone failed to start on non-Swarm daemon"
    fi
    stop_dnsweaver dm
}

# =============================================================================
# PHASE 2 — Lifecycle with real webhook calls
# =============================================================================

# Create expected workloads for a mode's lifecycle test. Each hostname is
# unique so assertions never cross-contaminate.
#
# Helper sequence per mode:
#   1. Start dnsweaver
#   2. Create workloads (service + container + failing container, as needed)
#   3. Assert POST /create for the ones that should be picked up
#   4. Assert absence for ones that should be filtered (task-containers,
#      failing container, or wrong-type-for-mode)
#   5. Remove workloads
#   6. Assert DELETE /delete arrives
#   7. Stop dnsweaver

phase2_mode_swarm() {
    log "=== lifecycle: mode=swarm ==="
    sink_reset
    start_dnsweaver dm swarm ready "$SINK_NAME"

    local H_SVC="web-sw-lc.${DOMAIN}"
    local H_CTR="tools-sw-lc.${DOMAIN}"

    info "deploy service with hostname label"
    dm service create --detach --name web-sw \
        --replicas 2 \
        --label dnsweaver.hostname="$H_SVC" \
        alpine sleep 99999 >/dev/null
    wait_for "2 task-containers for web-sw" 60 bash -c \
        "[[ \$(DOCKER_HOST=tcp://localhost:${MANAGER_PORT} docker ps -q --filter label=com.docker.swarm.service.name=web-sw | wc -l) -eq 2 ]]"
    sink_wait_for "POST /create for service hostname" POST "$H_SVC" 1 25

    info "deploy standalone container — should NOT be picked up in mode=swarm"
    local mark_ctr; mark_ctr=$(reconcile_mark)
    dm run -d --name tools-sw \
        --label dnsweaver.hostname="$H_CTR" \
        alpine sleep 99999 >/dev/null
    # In mode=swarm the watcher ignores container events, so reconcile
    # only fires on the 15s periodic timer. Wait for that and THEN prove
    # no CREATE happened — converts this from time-based to event-based.
    sink_assert_absent_after_reconcile \
        "standalone hostname ignored in mode=swarm" POST "$H_CTR" "$mark_ctr"

    info "remove service — expect DELETE for service hostname"
    dm service rm web-sw >/dev/null
    sink_wait_for "DELETE /delete for service hostname" DELETE "$H_SVC" 1 25

    dm rm -f tools-sw >/dev/null 2>&1 || true
    stop_dnsweaver dm
}

phase2_mode_standalone() {
    log "=== lifecycle: mode=standalone ==="
    sink_reset
    start_dnsweaver dm standalone ready "$SINK_NAME"

    local H_CTR="tools-st-lc.${DOMAIN}"
    local H_SVC="web-st-lc.${DOMAIN}"
    local H_FAIL="fail-st-lc.${DOMAIN}"

    info "deploy standalone container — should create record"
    dm run -d --name tools-st \
        --label dnsweaver.hostname="$H_CTR" \
        alpine sleep 99999 >/dev/null
    sink_wait_for "POST /create for standalone hostname" POST "$H_CTR" 1 25

    info "deploy service — task-containers must NOT create records (filter fix)"
    local mark_svc; mark_svc=$(reconcile_mark)
    dm service create --detach --name web-st \
        --replicas 2 \
        --label dnsweaver.hostname="$H_SVC" \
        alpine sleep 99999 >/dev/null
    wait_for "2 task-containers for web-st" 60 bash -c \
        "[[ \$(DOCKER_HOST=tcp://localhost:${MANAGER_PORT} docker ps -q --filter label=com.docker.swarm.service.name=web-st | wc -l) -eq 2 ]]"
    sink_assert_absent_after_reconcile \
        "task-containers filtered in mode=standalone" POST "$H_SVC" "$mark_svc"

    info "deploy failing container — should NEVER create a record"
    local mark_fail; mark_fail=$(reconcile_mark)
    dm run -d --name fail-st \
        --label dnsweaver.hostname="$H_FAIL" \
        alpine sh -c 'exit 1' >/dev/null
    sink_assert_absent_after_reconcile \
        "failing container not recorded" POST "$H_FAIL" "$mark_fail"

    info "remove standalone container — expect DELETE"
    dm rm -f tools-st >/dev/null
    sink_wait_for "DELETE /delete for standalone hostname" DELETE "$H_CTR" 1 25

    dm service rm web-st >/dev/null 2>&1 || true
    dm rm -f fail-st >/dev/null 2>&1 || true
    stop_dnsweaver dm
}

phase2_mode_both() {
    log "=== lifecycle: mode=both ==="
    sink_reset
    start_dnsweaver dm both ready "$SINK_NAME"

    local H_SVC="web-bo-lc.${DOMAIN}"
    local H_CTR="tools-bo-lc.${DOMAIN}"
    local H_FAIL="fail-bo-lc.${DOMAIN}"

    info "deploy 3-replica service — exactly one CREATE, not four"
    dm service create --detach --name web-bo \
        --replicas 3 \
        --label dnsweaver.hostname="$H_SVC" \
        alpine sleep 99999 >/dev/null
    wait_for "3 task-containers for web-bo" 60 bash -c \
        "[[ \$(DOCKER_HOST=tcp://localhost:${MANAGER_PORT} docker ps -q --filter label=com.docker.swarm.service.name=web-bo | wc -l) -eq 3 ]]"
    sink_wait_for "POST /create for service hostname" POST "$H_SVC" 1 25

    info "deploy standalone container — should also create record"
    dm run -d --name tools-bo \
        --label dnsweaver.hostname="$H_CTR" \
        alpine sleep 99999 >/dev/null
    sink_wait_for "POST /create for standalone hostname" POST "$H_CTR" 1 25

    info "exactly one CREATE for service hostname (no duplicates from tasks)"
    local svc_creates; svc_creates=$(sink_count POST "$H_SVC")
    if (( svc_creates == 1 )); then
        pass "single CREATE for service (task-containers not double-counted)"
    else
        fail "expected 1 CREATE for $H_SVC, saw $svc_creates"
    fi

    info "deploy failing container — no CREATE expected"
    local mark_fail; mark_fail=$(reconcile_mark)
    dm run -d --name fail-bo \
        --label dnsweaver.hostname="$H_FAIL" \
        alpine sh -c 'exit 1' >/dev/null
    sink_assert_absent_after_reconcile \
        "failing container not recorded" POST "$H_FAIL" "$mark_fail"

    info "remove service — expect DELETE for service hostname"
    dm service rm web-bo >/dev/null
    sink_wait_for "DELETE /delete for service hostname" DELETE "$H_SVC" 1 25

    info "remove standalone container — expect DELETE"
    dm rm -f tools-bo >/dev/null
    sink_wait_for "DELETE /delete for standalone hostname" DELETE "$H_CTR" 1 25

    dm rm -f fail-bo >/dev/null 2>&1 || true
    stop_dnsweaver dm
}

phase2() {
    phase "Phase 2 — Lifecycle with real provider calls"

    log "init single-node Swarm"
    dm swarm init --advertise-addr "$(dm inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$MANAGER_NAME" 2>/dev/null || true)" >/dev/null 2>&1 || \
        dm swarm init --advertise-addr 127.0.0.1 >/dev/null

    phase2_mode_swarm
    phase2_mode_standalone
    phase2_mode_both
}

# =============================================================================
# PHASE 3 — Non-manager topology: Swarm worker node
# =============================================================================
phase3() {
    phase "Phase 3 — Non-manager (Swarm worker)"

    log "start $WORKER_NAME on $NET_NAME"
    docker rm -f "$WORKER_NAME" >/dev/null 2>&1 || true
    docker run -d --privileged --name "$WORKER_NAME" \
        --network "$NET_NAME" \
        -p "${WORKER_PORT}:2375" \
        -e DOCKER_TLS_CERTDIR= \
        "$DIND_IMAGE" --host=tcp://0.0.0.0:2375 >/dev/null
    wait_for "worker daemon responsive" 30 dw info

    log "worker joins manager's Swarm as a worker"
    local worker_token manager_ip
    worker_token=$(dm swarm join-token -q worker)
    manager_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$MANAGER_NAME")
    dw swarm join --token "$worker_token" "${manager_ip}:2377" >/dev/null
    wait_for "worker shows as active, non-manager" 30 bash -c \
        "DOCKER_HOST=tcp://localhost:${WORKER_PORT} docker info --format '{{.Swarm.LocalNodeState}}/{{.Swarm.ControlAvailable}}' | grep -q '^active/false$'"

    log "ship dnsweaver image into the worker dind (docker save | docker load)"
    dm save "$DNSWEAVER_TAG" | dw load >/dev/null

    # On a worker, we can't use the sink by container name (sink lives in
    # the manager's daemon network, not reachable from the worker daemon).
    # For phase 3 we only test startup-guard behavior on mode=swarm/both —
    # those exit before any HTTP traffic — and for mode=standalone we only
    # need to verify it starts successfully (the unit tests already cover
    # label-filter behavior). So sink connectivity is unnecessary; we pass
    # a bogus host that dnsweaver will never reach.
    local BOGUS_SINK="sink-unreachable-in-phase3"

    log "mode=swarm on worker → ErrNotManager"
    if start_dnsweaver dw swarm exit "$BOGUS_SINK"; then
        if dw logs dnsweaver 2>&1 | grep -qi 'not.*manager\|not a manager'; then
            pass "mode=swarm rejected with ErrNotManager"
        else
            fail "mode=swarm exited but error wasn't ErrNotManager"
        fi
    else
        fail "mode=swarm did not exit on worker (it should have)"
    fi
    stop_dnsweaver dw

    log "mode=both on worker → ErrNotManager"
    if start_dnsweaver dw both exit "$BOGUS_SINK"; then
        if dw logs dnsweaver 2>&1 | grep -qi 'not.*manager\|not a manager'; then
            pass "mode=both rejected with ErrNotManager"
        else
            fail "mode=both exited but error wasn't ErrNotManager"
        fi
    else
        fail "mode=both did not exit on worker (it should have)"
    fi
    stop_dnsweaver dw

    log "mode=standalone on worker → should start (and apply task filter)"
    if start_dnsweaver dw standalone ready "$BOGUS_SINK"; then
        pass "mode=standalone runs on Swarm worker"
    else
        fail "mode=standalone failed to start on Swarm worker"
    fi
    stop_dnsweaver dw

    log "drain worker and remove from Swarm"
    dm node update --availability drain "$(dw info -f '{{.Name}}')" >/dev/null 2>&1 || true
    dw swarm leave >/dev/null 2>&1 || true
    dm node rm --force "$(dw info -f '{{.Name}}')" >/dev/null 2>&1 || true
}

# =============================================================================
# MAIN
# =============================================================================
start_manager
start_sink

log "build $DNSWEAVER_TAG inside dind-manager"
tar --exclude='.git' --exclude='node_modules' -C "$REPO_ROOT" -cf - . | \
    dm build -q -t "$DNSWEAVER_TAG" --build-arg VERSION=mode-both-dind - >/dev/null

phase1
phase2
if [[ $SKIP_PHASE3 -eq 0 ]]; then
    phase3
else
    log "phase 3 skipped (--skip-phase3)"
fi

phase "Summary"
printf '  passed: \033[1;32m%d\033[0m\n' "$PASSED"
printf '  failed: \033[1;31m%d\033[0m\n' "$FAILED"
if (( FAILED > 0 )); then
    exit 1
fi
pass "all phases passed"
