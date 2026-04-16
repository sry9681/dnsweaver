# Mixed Docker Mode (Swarm + Standalone)

`docker_mode: both` lets a single DNSWeaver instance on a Swarm manager
node pick up **Swarm services AND standalone containers** running on the
same daemon. This is useful when a manager also hosts dev/ops sidecars
launched via `docker run` that you want DNS records for.

## When to use it

- Single-node Swarm (`docker swarm init`) that also hosts `docker run`
  containers for adjacent tooling.
- Multi-node Swarm where the manager additionally hosts long-lived
  standalone containers (monitoring agents, local-only services).

Do **not** use it when standalone containers should be invisible to
DNSWeaver. The default (`auto`) still selects Swarm-only on a manager, so
no upgrade accidentally starts exposing previously-unmanaged containers.

## Preconditions

- Swarm is active on the local daemon (`docker info` shows
  `Swarm: active`).
- The local node is a manager (`ControlAvailable: true`).
- The socket (or socket proxy) exposes both the service and container
  APIs. See the ACL table below.

Violations produce a startup error:

- Swarm not active → `ErrSwarmNotActive`
- Local node is a worker → `ErrNotManager`

## Socket / socket-proxy ACL

| Endpoint                  | Method | Purpose                                                                |
|---------------------------|--------|------------------------------------------------------------------------|
| `/_ping`                  | GET    | Connectivity check                                                     |
| `/info`                   | GET    | Mode detection (Swarm state, manager role)                             |
| `/services`               | GET    | List Swarm services                                                    |
| `/services/{id}`          | GET    | Label inspection for a specific service                                |
| `/containers/json`        | GET    | List standalone containers                                             |
| `/containers/{id}/json`   | GET    | Label inspection for a specific container                              |
| `/events`                 | GET    | Real-time event stream (opened **twice** in `both` mode — see below)   |

Common socket-proxy (`tecnativa/docker-socket-proxy`) environment:

```
CONTAINERS=1
SERVICES=1
INFO=1
EVENTS=1
PING=1
```

The stream for `/events` is opened **twice concurrently** in `both` mode —
once for the `type=service` filter, once for `type=container`. Proxies
that rate-limit or cap concurrent long-poll connections on `/events` will
break `both`. If your proxy enforces a single-connection limit, either
raise it or use a direct socket mount.

## How task-container double-counting is prevented

Every Swarm service task is also a container with Swarm-managed labels
(`com.docker.swarm.service.id`, `com.docker.swarm.task.id`). Those
containers inherit the parent service's labels, including any
`dnsweaver.*` labels. Without filtering, each task replica would register
the same hostname that its parent service already registers — creating
duplicate entries, thrashing on every rolling update, and potentially
corrupting the provider mapping.

DNSWeaver filters out any container carrying
`com.docker.swarm.service.id` before composing the unified workload list.
The filter runs regardless of the configured mode whenever the local
daemon participates in an active Swarm — this also patches the
pre-existing issue where `docker_mode: standalone` on a manager would
double-count task-containers.

## Duplicate hostnames

If a Swarm service and a standalone container both declare the same
hostname, DNSWeaver creates a single DNS record and logs a
`HostnamesDuplicate` counter. Removing one of the two sources leaves the
record in place; only when *both* sources are gone is the record
orphan-cleaned.

The winning provider instance is deterministic: it is the first match in
provider configuration order. Record type and TTL come from that
instance's configuration, not from the workload that "got there first."

## Failure policy: fail-closed on partial listing failure

If either `ListServices` or `ListContainers` errors in `both` mode, the
entire listing call errors and the reconcile is skipped. Returning a
partial list would let the reconciler see "all containers gone" and
mass-delete DNS records.

The orphan-cleanup circuit breaker
(`internal/reconciler/orphan.go`, 50% threshold) is defense-in-depth, not
primary protection.

## Events and debouncing

During rolling Swarm updates you will see bursts of
`container start/stop/die/destroy` events from task-container
rescheduling. These reach the debouncer and trigger a reconcile, but
`ListWorkloads` filters the task-containers out, so the reconcile is a
no-op. The default `DebounceInterval` (2 s) absorbs normal bursts. On
very large clusters with frequent deployments, raise
`DebounceInterval`.
