# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **`docker_mode: both`** — on a Swarm manager, simultaneously discover
  Swarm services and standalone containers. Swarm task-containers are
  filtered out via the `com.docker.swarm.service.id` label to prevent
  double-counting with their parent service. Event watching uses two
  independent `/events` subscriptions (one per type) to avoid Docker's
  inconsistent cross-type action-filter semantics across daemon and
  socket-proxy versions. See `docs/deployment/mixed-mode.md`.
- `Client.IsSwarmActive()` — reports local Swarm participation
  independent of listing behavior. Used by the task-container filter so
  the correctness fix applies to both `ModeBoth` and legacy
  `ModeStandalone`-on-manager configurations.

### Fixed
- **`docker_mode: standalone` on a Swarm manager** no longer
  double-counts Swarm task-containers alongside their parent service's
  records. The filter is gated on local Swarm liveness, not on
  configured mode, so any manager node running in `standalone` mode
  inherits the fix without configuration changes.
- **Technitium HTTPS/SVCB records no longer break zone listing.**
  Recent Technitium versions return `svcParams` as a nested JSON object
  (e.g. `{"alpn":{"alpnIds":["h2"]}}`) from `zones/records/get`, while
  older versions and the add/delete endpoints use the pipe form
  (`alpn|h2`). The previous string-typed field failed to unmarshal the
  object form, which aborted the entire zone listing and blocked
  reconcile for A/TXT records that DNSWeaver manages. `svcParams` now
  accepts both shapes and normalizes to the pipe form internally;
  unknown keys are preserved deterministically so record identity
  stays stable across reconciles.

### Notes
- `docker_mode: auto` behavior is **unchanged**: managers continue to
  auto-detect to Swarm-only. To opt in to mixed discovery, set
  `docker_mode: both` explicitly.
- `ListWorkloads` fails closed on partial listing failure in `both`
  mode: if either `/services` or `/containers/json` errors, the whole
  call errors and reconcile is skipped. Returning a partial list would
  let the reconciler mass-delete records for the failing side.

## [1.1.3] - 2026-04-10

### Security
- **CACHE_BUST build arg for Docker layer cache invalidation**: `--pull` alone was
  insufficient — if the Alpine base image tag hasn't been rebuilt, Docker layer
  caching preserves stale `apk upgrade` output. Added `ARG CACHE_BUST` in the
  runtime stage and `--build-arg CACHE_BUST=$CI_PIPELINE_ID` to all CI Docker
  build commands, ensuring every pipeline runs a fresh `apk upgrade`
- **Reconciler race condition**: Added `reconcileMu` mutex to serialize
  `Reconcile()` calls, preventing concurrent map access
- **Case-sensitive hostname comparison**: Fixed orphan cleanup to use
  `source.NormalizeHostname()` for consistent case-insensitive hostname matching
- **SSH config `getEnvOrFile` alignment**: When `_FILE` key is set but file is
  unreadable, now returns empty string (hard-fail) matching config behavior
- **Dry-run orphan accuracy**: Always build record cache (was nil in dry-run mode);
  refactored deletion functions to check dry-run per-record
- **RecoverOwnership error handling**: Now returns error listing failed providers
  instead of silently continuing
- **Bounded HTTP response reading**: Replaced all `io.ReadAll` calls in Pi-hole
  client with `httputil.ReadBody` (10 MB limit) to prevent memory exhaustion
- **Integer overflow guards**: Added `gosec G115` clamps — TTL to `uint32` in
  RFC 2136, SRV/HTTPS fields to `uint16` in Technitium

### Fixed
- **Hostname provider map initialization**: Initialize `hostnameProviders` map in
  `New()` instead of lazy nil check

## [1.1.2] - 2026-04-10

### Security
- **Docker build: pull fresh base images**: Added `--pull` to all `docker build`
  commands in CI/CD pipeline to prevent Docker layer caching from reusing Alpine
  images with stale OpenSSL packages (CVE-2026-28390)

## [1.1.1] - 2026-04-10

### Fixed
- **MkDocs build warning**: Fixed broken relative link to CHANGELOG in
  `testing/RELEASE-CHECKLIST.md` that caused GitHub Actions doc build to fail

### Changed
- **Multi-instance docs**: Added section explaining how `DNSWEAVER_INSTANCE_ID`
  interacts with providers that use target-based ownership inference (AdGuard Home,
  Pi-hole file mode, dnsmasq)

## [1.1.0] - 2026-04-10

### Added
- **AdGuard Home provider**: New DNS provider supporting AdGuard Home's DNS Rewrite
  API. Supports A, AAAA, and CNAME records with full CRUD lifecycle, native
  in-place updates via `PUT /control/rewrite/update`, and Basic Auth. Requires
  AdGuard Home v0.107+ ([#77](https://github.com/maxfield-allison/dnsweaver/issues/77))
- **Target-based ownership inference**: Managed-mode orphan cleanup now works for
  providers that cannot store TXT ownership records (AdGuard Home, Pi-hole file mode,
  dnsmasq). When a record's type and target match the provider instance's configured
  values, dnsweaver infers ownership and cleans up the record. Records with different
  targets are preserved, protecting manually-created entries

### Changed
- **Startup warning for non-TXT providers**: Updated log message from
  "ownership tracking unavailable" to "managed mode will use target-based
  ownership inference" — managed mode is now fully functional for these providers

## [1.0.5] - 2026-04-08

### Fixed
- **Companion HTTPS records via `NewWithHTTPClient()`**: `autoHTTPSRecords` and
  `autoHTTPSALPN` config fields were not propagated when constructing Technitium
  providers with a custom HTTP client, causing companion HTTPS record creation to
  silently fail in that code path

### Security
- **Go 1.25.0 → 1.25.9**: Fixes three crypto stdlib CVEs:
  - GO-2026-4947 — crypto/x509 chain building DoS
  - GO-2026-4946 — crypto/x509 policy validation DoS
  - GO-2026-4870 — crypto/tls KeyUpdate DoS
- **CI govulncheck allowlist**: Improved documentation for docker/docker SDK
  vulns (GO-2026-4887, GO-2026-4883) — daemon-side AuthZ issues, not
  exploitable via client SDK imports; no upstream fix available

## [1.0.4] - 2026-04-02

### Security
- **Alpine 3.23 base image**: Runtime base image upgraded from Alpine 3.21 to 3.23
  for reduced CVE surface and latest security patches
- **CI security hardening**: `security:trivy` (filesystem scan) and
  `security:govulncheck` now block pipeline on CRITICAL/HIGH findings instead
  of running in warn-only mode. `govulncheck` uses a wrapper that allows
  known-unfixed upstream vulnerabilities (docker/docker SDK) while blocking
  on any new findings
- **SECURITY.md**: Added responsible vulnerability disclosure policy with
  supported versions, reporting process, and security practices

### Changed
- Alpine base image upgraded from 3.21 to 3.23
- `.trivyignore` entries now include explicit review dates

## [1.0.3] - 2026-03-31

### Security
- **Alpine 3.20 → 3.21**: Resolves 14 CVEs in base image packages (10 openssl
  including CVE-2025-15467 RCE, 3 busybox, 1 zlib)
- **Remove wget from runtime image**: Eliminates 2 HIGH CVEs
  (CVE-2025-69194 path traversal, CVE-2024-10524 SSRF) and reduces attack
  surface. Healthcheck now uses busybox `nc` instead of wget.
- **Container image scanning**: Added Trivy container image scan to CI/CD
  pipeline — CRITICAL/HIGH CVEs now block releases automatically

### Changed
- CI validation job images updated from Alpine 3.20 to 3.21

## [1.0.2] - 2026-03-31

### Added
- **Companion HTTPS Records (Technitium)**: Auto-creates HTTPS (SVCB Type 65)
  companion records alongside A/AAAA/CNAME records to prevent ECH fallback
  errors in split-horizon DNS environments (#158)
  - Enabled by default (`AUTO_HTTPS_RECORDS=true`); set `false` to disable
  - Configurable ALPN protocol via `AUTO_HTTPS_ALPN` (default: `h2`)
  - Skips creation if HTTPS record already exists (safe for manual records)
  - Lifecycle-managed: companion record deleted when parent record is removed
- **HTTPS Record Type**: Added `RecordTypeHTTPS` with `HTTPSData` struct to
  provider type system
- **ECH Troubleshooting**: FAQ entry for Firefox/Chrome ECH connection failures

### Documentation
- **Companion HTTPS Guide**: Full section in Technitium provider docs with
  Why/What/Behavior/Configuration subsections
- **Split-Horizon Tip**: Added companion HTTPS recommendation to split-horizon
  deployment guide
- **Config Example**: Added `auto_https_records` and `auto_https_alpn` to
  example configuration file

## [1.0.1] - 2026-03-30

### Documentation
- **Platform Parity**: Comprehensive docs refresh positioning Docker and
  Kubernetes as co-equal first-class platforms across all pages
- **Landing Page Rewrite**: Tabbed quick start (Docker Compose, Helm, Kustomize),
  updated flowchart with Kubernetes sources, comparison table vs external-dns
- **Getting Started Overhaul**: Dual Docker/K8s paths with tabbed prerequisites,
  installation, configuration, secrets, and verification steps
- **Secrets Management**: Renamed from "Docker Secrets"; added Kubernetes Secrets
  section (secretKeyRef, volume mounts, external secret operators)
- **Observability**: Added Kubernetes monitoring section (ServiceMonitor, pod
  probes, Helm chart integration)
- **FAQ Expansion**: Added Kubernetes-specific entries (RBAC, namespace filtering,
  dual-platform configuration, troubleshooting)
- **Development Guide**: Added Kubernetes dev workflow (kind cluster, local testing)
- **RFC 5737 IPs**: Standardized all example IPs from private ranges to
  documentation-reserved ranges (192.0.2.x, 198.51.100.x) across 30+ files
- **SEO Improvements**: Updated site metadata, page titles, and descriptions for
  better discoverability of Kubernetes-related searches

## [1.0.0] - 2026-03-25

### Added
- **Config Validation CLI**: `--validate` flag for pre-flight config checks with
  structured error reporting (#71)
- **Graceful Shutdown**: In-flight reconciliation tracking with clean provider
  teardown (#69)
- **Structured Logging**: `slog`-based logging with JSON/text format and file
  rotation support (#67)
- **`dnsweaver.hostnames` Label**: Comma-separated hostname declarations for
  Docker containers (#96)
- **Dual-Stack DNS Guide**: Deployment documentation for IPv4/IPv6 environments
- **Source/Watcher Metrics**: Prometheus instrumentation for source discovery and
  watcher activity (#97)

### Changed
- **Pi-hole Config**: `MODE` renamed to `ACCESS_MODE`; `DNSWEAVER_SOURCE` (singular)
  deprecated in favor of `DNSWEAVER_SOURCES` (plural) (#93)
- **`instance_id` Restructured**: Moved to top-level config field (#93)

### Fixed
- **Orphan Cleanup**: Hostname-provider mapping tracked correctly during provider
  switches (#51)
- **Startup Race**: Events during initial reconciliation no longer lost (#55)
- **Health Recovery**: Health checker registered for recovered providers (#127)
- **Pi-hole Default**: `ACCESS_MODE` defaults to `api` when not specified (#98)

### Security
- **Pre-v1.0 Security Audit**: Comprehensive hardening including HTTP response
  body limits, shell metacharacter validation, SSH credential handling, and input
  sanitization (#36)
- **Code Review**: Dead code removal, error handling improvements, naming
  consistency (#94)

### Testing
- **Shared Test Harness**: Mock infrastructure and helper utilities for provider
  testing (#111)
- **Reconciler Edge Cases**: Failure, behavior, and observability tests (#77)
- **RFC 2136 Integration Tests**: Full CRUD lifecycle testing (#130)
- **Standardized Test Templates**: Reusable integration testing frameworks (#136)
- **Integration Tested**: Verified against Technitium DNS and Cloudflare in
  multi-provider E2E scenarios

### Documentation
- **Architecture Overview**: System design and component interaction docs (#35)
- **Multi-Instance Guide**: Running multiple dnsweaver instances (#35)
- **SSH Remote Management**: dnsmasq provider SSH docs and secrets guide (#99)
- **Test Case Matrix**: Release checklist and test coverage mapping (#109)
- **Provider Documentation**: Accuracy corrections across all providers

## [0.9.3] - 2026-03-11

### Fixed
- **Source Registry**: `dnsweaver.enabled=false` (Docker) and `dnsweaver.dev/enabled=false`
  (K8s) now opt out of ALL sources at the registry level, not just the dnsweaver native
  source. Previously the traefik source still extracted hostnames from disabled workloads.
  Fixes [#75](https://github.com/maxfield-allison/dnsweaver/issues/75),
  [#152](https://gitlab.bluewillows.net/root/dnsweaver/-/issues/152).
- **Helm Chart**: Bumped appVersion to 0.9.3

## [0.9.2] - 2026-03-11

### Fixed
- **Critical Correctness** (#148):
  - `parseIntEnv` replaced with `strconv.Atoi` to prevent silent integer overflow on 32-bit platforms
  - `formatSRVKey` uses `fmt.Sprintf` instead of `string(rune(int))` which produced Unicode characters instead of numeric keys
  - Thread-safe DNS record `Catalog` — all public methods now protected by `sync.Mutex`
  - Enum validation for `log_level`, `log_format`, and `docker_mode` config fields
  - `RemoveHostname` normalizes hostname before operations (RFC 1035)
  - A record validation rejects IPv6 addresses (must use AAAA)
  - `GetExistingRecords` uses case-insensitive comparison per RFC 1035
- **Concurrency & Safety** (#149):
  - `_FILE` secret read failure is now a hard error — no longer silently falls through to direct env var
  - Signal handler registered early (before initialization) so SIGINT/SIGTERM during startup triggers graceful shutdown
  - Reconciliation concurrency guard — `TryLock` prevents overlapping reconciliation from timer + events
  - `SetEnabled`/`SetDryRun` use `atomic.Bool` for thread-safe access from concurrent goroutines
  - Mass deletion circuit breaker — orphan cleanup aborts if >50% of known hostnames would be deleted
  - Kubernetes `AddEventHandler` errors now propagated (previously logged and silently dropped)
  - `sync.WaitGroup` ensures periodic reconciliation goroutine completes during shutdown
- **Provider & Security Hardening** (#150):
  - dnsmasq `GetServer()` uses `net.SplitHostPort` — correct IPv6 address parsing
  - Domain matcher strips trailing dots before comparison for consistent matching
  - Pi-hole v6 URL path values escaped with `url.PathEscape` to prevent injection
  - dnsmasq reload command validated against shell metacharacters
  - SSH `RunWithSudo` pipes password via stdin instead of `echo` (no `/proc` exposure)
  - HTTP response bodies capped at 10 MB via `httputil.ReadBody` across all providers
- **Documentation Alignment** (#151):
  - Added dnsmasq provider example to `config.example.yml`
  - Documented `_FILE` suffix support for `TSIG_SECRET` and `SSH_PASSWORD`
  - Fixed `DNSWEAVER_SOURCE` (singular) → `DNSWEAVER_SOURCES` (plural) in K8s deployment docs
  - Documented Kubernetes source auto-registration behavior
  - `record-type` annotation and K8s source doc consistently list A, AAAA, CNAME, SRV, TXT
- **Helm Chart**: Bumped appVersion to 0.9.2

## [0.9.1] - 2026-03-09

### Fixed
- **Documentation**: Comprehensive documentation review and corrections
  - Fixed incorrect env var `TLS_SKIP_VERIFY` → `INSECURE_SKIP_VERIFY` in FAQ
  - Added `rfc2136` to provider TYPE list in environment variable reference
  - Added missing per-instance env vars to reference: `MODE`, `EXCLUDE_DOMAINS_REGEX`,
    `INSECURE_SKIP_VERIFY`
  - Added `Operational Modes` page (`modes.md`) to mkdocs navigation
  - Updated Go version from 1.24+ to 1.25+ in contributing guide
  - Updated project structure in contributing guide (added `sources/`, `internal/kubernetes/`,
    `pkg/workload/`, `pkg/sshutil/`, `providers/rfc2136/`; removed obsolete `internal/sources/`)
  - Added 11 missing CHANGELOG version comparison links (v0.3.1–v0.7.0)
  - Used standard documentation IPs in Kubernetes source docs and code examples
  - Fixed placeholder GitLab social link in mkdocs.yml
  - Added `instance_id` field and RFC 2136 provider example to config.example.yml
- **Helm Chart**: Bumped chart version to 0.2.0 and appVersion to 0.9.1

## [0.9.0] - 2026-03-08

### Added
- **Kubernetes Platform Support**: Full Kubernetes-native DNS management
  - K8s watcher with informer-based event watching (#138) — real-time detection
    of Ingress, IngressRoute (Traefik CRD), HTTPRoute (Gateway API), and Service resources
  - K8s source with annotation-driven configuration (#139) — hostnames, provider hints,
    TTL, proxied, and metadata via `dnsweaver.dev/*` annotations
  - Deployment manifests: Helm chart, Kustomize base, and raw RBAC manifests (#140)
  - Comprehensive Kubernetes deployment and source documentation
  - Platform selector (`DNSWEAVER_PLATFORM`) — run in `docker`, `kubernetes`, or `both` mode
- **Per-Record Metadata System** (#141): Extensible key-value metadata on DNS records
  - `Metadata map[string]string` field on `Record` and `RecordHints` (Phase 2)
  - Cloudflare per-record proxied control via `Record.Metadata["proxied"]` (Phase 3)
  - Source-level proxied field and `meta.*` label parsing in dnsweaver source (Phase 4)
  - Metadata persistence in ownership TXT records (Phase 5)
  - Metadata recovery from ownership TXT on startup for reconciliation (Phase 6)
- **Workload Abstraction** (#137): Platform-agnostic workload interface replacing
  Docker-specific container/service types — enables multi-platform source support

### Changed
- **Cloudflare proxied default**: Changed from `false` to `true` to match Cloudflare's
  own default behavior — new records are proxied unless explicitly disabled

### Fixed
- **CI/CD**: Bumped Go version in CI pipeline and Dockerfile to 1.25 to match `go.mod`

## [0.8.1] - 2026-02-27

### Fixed
- **Cloudflare Provider**: Fix JSON parsing failure on API responses — Cloudflare
  changed `messages` field from `[]string` to `[]object` (matching `errors` shape),
  causing `json: cannot unmarshal object into Go struct field` on every API call

## [0.8.0] - 2026-02-27

### Added
- **RFC 2136 Dynamic DNS Provider** (#132): Industry-standard DNS update protocol support
  - Works with BIND, Windows DNS, PowerDNS, Knot DNS, Technitium, and any RFC 2136-compliant server
  - TSIG authentication with HMAC-SHA256/SHA512 support
  - Catalog-based hostname enumeration via `_dnsweaver-catalog-N.<zone>` TXT records
  - Per-record ownership verification via `_dnsweaver.<hostname>` TXT records
  - Supports A, AAAA, CNAME, and SRV record types
  - No AXFR required — catalog provides efficient O(n) enumeration
- **Multi-Instance Coordination** (#84): Instance-scoped ownership for shared DNS zones
  - New `DNSWEAVER_INSTANCE_ID` config (env var + YAML) identifies each instance
  - Ownership TXT records now carry instance ID: `heritage=dnsweaver,instance=<id>`
  - Each instance only manages its own records — no cross-instance interference
  - Orphan cleanup is instance-scoped: removing a service only cleans that instance's records
  - Fully backward compatible: empty instance ID preserves legacy single-instance behavior
  - Enables multiple dnsweaver deployments (e.g., per-node Docker, Pi clusters) sharing a zone

## [0.7.0] - 2026-01-19

### Added
- **Pi-hole v6 API Support** (#74): Full support for Pi-hole v6 REST API
  - Automatic version detection probes Pi-hole to determine API version
  - Session-based authentication with SID tokens for v6
  - Supports `dns.hosts` and `dns.cnameRecords` config endpoints
  - New `API_VERSION` config option to override auto-detection (`v5`/`v6`/`auto`)
  - Maintains full backward compatibility with Pi-hole v5 legacy API
- **Graceful Provider Initialization** (#125): Resilient startup when providers are unavailable
  - Providers that fail to connect at startup are retried in the background
  - dnsweaver starts in "degraded" mode and becomes healthy once providers recover
  - Configurable retry interval and max attempts
  - Ping verification ensures provider connectivity before marking ready
- **Shared SSH/SFTP Client Package** (#120): New `pkg/sshutil` for remote provider operations
  - SSH client with connection pooling and keepalive
  - SFTP-based FileSystem interface for remote file operations
  - SSH exec-based CommandRunner for remote commands
  - Configuration loading with Docker secrets support (`_FILE` pattern)
  - Foundation for remote dnsmasq, BIND, and hosts-file providers

### Fixed
- **Pi-hole v6 version detection** (#126): Use correct `/api/info/login` endpoint
  - Pi-hole v6 does not have `/api/info` endpoint (returns 404)
  - Changed to probe `/api/info/login` which is unauthenticated and confirms v6
- **Pi-hole v6 List() returns 0 records** (#103): Correct API response parsing
  - Pi-hole v6 `/api/config/dns` returns hosts nested under `config.dns`, not `config`
  - Fixed parsing to handle `{ "config": { "dns": { "hosts": [...] } } }` structure
  - Resolves "Item already present" errors when records already existed

## [0.6.0] - 2026-01-15

### Added
- **MkDocs Documentation Site** (#65): Comprehensive documentation with Material theme
  - Full API reference, configuration guides, and provider documentation
  - Searchable, mobile-friendly documentation hosted via GitHub Pages
  - Includes examples, quickstart guides, and architecture overview

### Changed
- **HTTP Client Consolidation** (#92): Refactored HTTP client logic into shared `pkg/httputil` package
  - All providers now use consistent HTTP client configuration
  - Centralized TLS settings, timeouts, and error handling
  - Added comprehensive test coverage for HTTP client behavior
  - Providers use new factory pattern for cleaner initialization

## [0.5.3] - 2026-01-15

### Fixed
- **INSECURE_SKIP_VERIFY env var not working** (#95, GitHub #74): Fixed environment variable not being loaded
  - `DNSWEAVER_{INSTANCE}_INSECURE_SKIP_VERIFY=true` was silently ignored due to missing field in config loader
  - Environment variable now correctly propagates to the HTTP client TLS configuration
  - Thanks to @jaykumar2001 for reporting this issue

## [0.5.2] - 2026-01-14

### Added
- **INSECURE_SKIP_VERIFY for Technitium** (#86): Skip TLS certificate verification for self-signed certs
  - Configure via `DNSWEAVER_{INSTANCE}_INSECURE_SKIP_VERIFY=true`
  - Enables connections to HTTPS endpoints using IP addresses or self-signed certificates
  - Logs security warning when enabled
  - HTTP client consolidation planned in #92

### Fixed
- **dnsweaver.enabled=false label ignored** (#89): Services with `dnsweaver.enabled=false` now correctly skip record creation
  - Global `dnsweaver.enabled=false` prevents all record creation for the workload
  - Per-record `dnsweaver.records.<name>.enabled=false` disables specific named records
- **dnsweaver.ttl label ignored for simple hostname** (#90): TTL override now works in simple hostname mode
  - `dnsweaver.ttl=60` now correctly sets TTL when using `dnsweaver.hostname`
  - Previously only worked with named records (`dnsweaver.records.<name>.ttl`)

## [0.5.1] - 2026-01-13

### Added
- **Environment Variable Override for YAML Configs** (#67): Inject secrets into YAML-based provider configs
  - Provider-specific env vars override YAML config values: `DNSWEAVER_{PROVIDER}_{FIELD}`
  - Secret fields support `_FILE` suffix for Docker/Kubernetes secrets: `DNSWEAVER_{PROVIDER}_TOKEN_FILE`
  - Secret fields: TOKEN, API_KEY, AUTH_TOKEN, PASSWORD
  - Non-secret fields: URL, ZONE, ZONE_ID, API_EMAIL, TARGET, TTL, MODE
  - Allows YAML configs to be version-controlled safely without secrets
  - See `docs/examples/` for configuration and deployment examples
- **Reorganized Example Documentation**: Moved examples to `docs/examples/` folder
  - `config.example.yml` - Complete YAML configuration reference
  - `docker-compose.dev.example.yml` - Local development setup
  - `docker-stack.example.yml` - Production Swarm deployment
  - `docker-stack-testing.example.yml` - Testing stack with Docker secrets
  - `docker-entrypoint.sh` - Entrypoint wrapper for config templating

### Changed
- Refactored `loadInstanceConfig` to use shared `providerConfigFields` for consistency

## [0.5.0] - 2026-01-13

### Added
- **YAML Configuration File Support** (#66): Full YAML config file support
  - Load configuration from YAML file via `DNSWEAVER_CONFIG` env var or `--config` flag
  - Environment variable interpolation with `${VAR}` and `${VAR:-default}` syntax
  - Configuration priority: env vars > config file > defaults
  - Example config file at `docs/config.example.yml`
  - Supports all existing configuration options in structured YAML format
- **Version Flag**: Added `--version` flag to display version and build date
- **Provider Capabilities Interface** (#79): Providers report their capabilities
  - `SupportsOwnershipTXT` — whether provider can create TXT records for ownership
  - `SupportsNativeUpdate` — whether provider implements `Updater` interface
  - `SupportedRecordTypes` — list of record types the provider handles (A, AAAA, CNAME, SRV)
- **Updater Interface** (#70): Optional provider interface for native record updates
  - Providers implementing `Updater` can update records in-place without delete+create
  - Reconciler automatically falls back to delete+create for providers without native update
  - Technitium provider implements native update support
- **Per-Instance Operational Modes** (#80): Control how dnsweaver manages records per provider
  - `managed` (default) — only touch records dnsweaver created (with ownership TXT)
  - `authoritative` — full control over configured scope; deletes unmatched in-scope records
  - `additive` — write-only mode; never deletes any records
  - Configure via `DNSWEAVER_{INSTANCE}_MODE` environment variable
- **Comprehensive Test Coverage** (#68): Core reconciler coverage increased from 26% to 83%+
  - Added tests for reconciler, watcher, provider registry, and error handling
  - Edge case coverage for debouncing, lifecycle, and event filtering

### Changed
- **Reconciler Refactored** (#78): Split monolithic reconciler into focused modules
  - `reconciler.go` — main loop and orchestration (~300 lines)
  - `actions.go` — create/update/delete operations
  - `comparison.go` — record diffing with `CompareRecordSets()` helper
  - `orphan.go` — orphan detection and cleanup
  - `ownership.go` — TXT record ownership tracking
  - `cache.go` — provider state caching
  - Each module under 400 lines for maintainability

## [0.4.2] - 2026-01-12

### Fixed
- **Lint Compliance**: Resolved all golangci-lint issues for stricter configuration
  - Fixed gofmt formatting across 45 files
  - Fixed exhaustive switch statements (RecordType, Validator interface)
  - Fixed errorlint issues (use `errors.Is` instead of direct comparison)
  - Fixed variable shadowing in dnsmasq/Pi-hole providers
  - Fixed typos (cancelled → canceled)
  - Added status constants for health checks and provider metrics

### Changed
- **Linter Configuration**: Refined `.golangci.yml` for long-term maintainability
  - Disabled `prealloc` (micro-optimization not worth verbosity)
  - Disabled `revive:unexported-return` (intentional API pattern)
  - Added structured exclusions for tests, providers, and config
  - Enabled only diagnostic and performance gocritic tags
- **Contributing Guide**: Fixed internal GitLab URL to public GitHub URL

## [0.4.1] - 2026-01-11

### Added
- **CLEANUP_ON_STOP Option**: New `DNSWEAVER_CLEANUP_ON_STOP` configuration option (default: `true`)
  - When `true` (default): DNS records are deleted when containers stop or are removed
  - When `false`: DNS records are only deleted when containers are removed, not when stopped
  - Useful for containers that frequently stop/start and don't need DNS cleanup on stop
- **Native dnsweaver Labels** (#27): Use dnsweaver without Traefik dependency
  - New label format: `dnsweaver.hostname`, `dnsweaver.type`, `dnsweaver.target`
  - Works alongside existing Traefik label parsing
  - Enables DNS management for services that don't use Traefik
- **Pi-hole Provider** (#15): Native Pi-hole DNS integration with two operation modes
  - **API mode**: Uses Pi-hole's Admin API (recommended for Pi-hole v5)
    - Manages Local DNS Records (A/AAAA) and Local CNAME Records
    - Authentication via admin password (supports `_FILE` suffix for secrets)
  - **File mode**: Direct file manipulation for containerized Pi-hole setups
    - Uses dnsmasq config format internally
    - Configurable config directory, filename, and reload command
  - Supports A, AAAA, and CNAME record types
  - Zone filtering for multi-zone environments
  - **Note**: Pi-hole v6+ uses a different API; see #74 for v6 support
- **dnsmasq Provider** (#28): File-based DNS provider for dnsmasq DNS server
  - Manages records by writing to dnsmasq configuration files
  - Supports `address=` directive for A/AAAA records
  - Supports `cname=` directive for CNAME records
  - Automatic dnsmasq reload after changes (configurable)
  - Serves as foundation for Pi-hole integration
  - Configurable config directory, filename, and reload command
  - **Note**: Orphan cleanup limited due to lack of TXT ownership support; see #73
- **SRV Record Support** (#62): Service discovery DNS records
  - Added `SRV` record type for service discovery (Minecraft, SIP, LDAP, XMPP)
  - SRV records include priority, weight, port, and target fields
  - SRV naming convention: `_service._proto.name` (e.g., `_minecraft._tcp.example.com`)
  - Full support across all providers: Technitium, Cloudflare, Webhook
  - Updated README with SRV record type in reference table
- **AAAA Record Support** (#63): IPv6 DNS record support
  - Added `AAAA` record type for IPv6 addresses alongside existing `A` (IPv4) and `CNAME` types
  - Strict validation: A records require IPv4, AAAA records require IPv6, CNAME requires hostname
  - Full support across all providers: Technitium, Cloudflare, Webhook
  - Updated README with IPv6 configuration examples

### Fixed
- **Cache includes all record types** (#63, #62): Record cache now properly includes AAAA and SRV records
  - Previously, `getExistingRecords()` only cached A and CNAME records
  - SRV and AAAA records were being missed during orphan cleanup
- **Orphan cleanup uses correct record type** (#63, #62): Delete operations now use the actual record type
  - Previously, orphan cleanup always used `A` record type for deletion regardless of actual type
  - Now correctly deletes AAAA records as AAAA and SRV records as SRV
- **SRV record data updates**: Fixed multiple issues with SRV record lifecycle
  - Proper detection of SRV record data changes (priority, weight, port, target)
  - Correct API parameter names for Technitium SRV records
  - SRV data properly passed through reconciler to providers
  - RFC 2782 validation for SRV record hostnames

## [0.3.3] - 2026-01-09

### Added
- **Periodic Reconciliation Timer**: Implemented the missing periodic reconciliation loop
  - Uses `DNSWEAVER_RECONCILE_INTERVAL` setting (default: 60 seconds)
  - Acts as a safety net for any missed Docker events
  - Ensures containers with slow restarts don't get their DNS records deleted prematurely
  - The config value existed since v0.1.0 but the timer was never wired up (oversight in initial implementation)

### Changed
- **Package Structure Refactor** (#61): Moved source implementations to root-level `sources/` directory
  - `pkg/source/traefik/` → `sources/traefik/` for consistency with `providers/` structure
  - Import path changed: `gitlab.bluewillows.net/root/dnsweaver/sources/traefik`
  - Internal interfaces remain in `pkg/source/` (no breaking changes for external consumers)

### Fixed
- **CI: Trivy security scan fails** (#59): Fixed container entrypoint issue
  - The `aquasec/trivy:latest` image has trivy as entrypoint, causing "unknown command sh" error
  - Added explicit entrypoint override in GitLab CI configuration
- **CI: Lint job errors** (#60): Fixed all golangci-lint errors
  - Fixed unchecked error returns in test files (errcheck)
  - Fixed deprecated Docker types: `types.ServiceListOptions` → `swarm.ServiceListOptions` (staticcheck SA1019)
  - Removed unused `printUsage` function and mock types
  - Fixed unnecessary nil check before len() (gosimple S1009)

## [0.3.2] - 2026-01-09

### Fixed
- **DNSWEAVER_ADOPT_EXISTING not working** (#58): Environment variable was parsed but not passed to reconciler
  - The value was correctly loaded from environment but was missing from reconciler config initialization
  - Now `DNSWEAVER_ADOPT_EXISTING=true` works as documented
  - Added `adopt_existing` to startup log for easier debugging
  - Thanks to u/pheitman on Reddit for reporting this bug

## [0.3.1] - 2026-01-09

### Added
- **Hostname Validation** (#49): RFC 1123 hostname validation before DNS operations
  - Validates label length (max 63 chars) and total hostname length (max 253 chars)
  - Checks for valid characters (alphanumeric and hyphens)
  - Rejects empty labels, leading/trailing hyphens, special characters
  - Supports wildcards (`*.example.com`) in first label only
  - Invalid hostnames are logged with warnings and skipped (won't fail reconciliation)
  - New `HostnamesInvalid` counter in reconciliation results
- **Adopt Existing Setting** (#58): Control whether dnsweaver adopts existing DNS records
  - New `DNSWEAVER_ADOPT_EXISTING` environment variable (default: `false`)
  - When false, existing records without ownership TXT are left unmanaged
  - When true, dnsweaver creates ownership TXT to adopt matching records
  - Prevents surprising behavior where dnsweaver silently takes over manually-created records
  - Thanks to u/pheitman on Reddit for testing and feedback on this feature
- **Duplicate Hostname Detection** (#54): Warn when same hostname appears in multiple workloads
  - Logs warning with both workload names when duplicate hostname detected
  - First workload wins (deterministic, alphabetical by service discovery order)
  - New `HostnamesDuplicate` counter in reconciliation results

### Documentation
- **Domain Pattern Overlap** (#52): Documented multi-provider matching behavior
  - Clarified that hostnames are sent to ALL matching providers (split-horizon DNS design)
  - Added examples for non-overlapping patterns using `EXCLUDE_DOMAINS`
  - Documented that instance order doesn't affect provider selection
- **TTL Handling** (#46): Documented TTL configuration and provider-specific behavior
  - Added TTL handling section explaining caching behavior
  - Documented Cloudflare quirks: proxied records use "Automatic" TTL (ignores configured value)
  - Clarified that TTL changes require record deletion/recreation

## [0.3.0] - 2026-01-08

### Added
- **IP Change Detection** (#43, #44): Reconciler now detects when a DNS record exists with a different target
  - Updates records in-place instead of failing with conflict errors
  - Logs `updated record` with old and new target values
  - Handles A→CNAME and CNAME→A type conflicts by deleting and recreating
- **Provider Record Caching**: Cache DNS records per reconciliation cycle
  - Reduces API calls by querying each provider once per cycle
  - Significant performance improvement for large deployments
  - Cache automatically invalidated between reconciliation runs
- **Environment Variable Rename**: `DNSWEAVER_PROVIDERS` → `DNSWEAVER_INSTANCES`
  - Clarifies that instance names are arbitrary identifiers, not provider types
  - Old variable still works with deprecation warning
  - README and examples updated to use new naming

### Fixed
- **Technitium**: Detect "Identical record" response as conflict error (#56)

## [0.2.1] - 2026-01-07

### Fixed
- **CI/CD**: GitHub mirror now preserves commit history instead of force-pushing
  - Clones existing GitHub repo before applying changes
  - Only force-pushes tags (for re-releases), not the main branch
  - New releases now appear as proper commits on top of history

## [0.2.0] - 2026-01-07

### Added
- **Cloudflare DNS Provider**: Public DNS management via Cloudflare API (#24)
  - API token authentication (scoped tokens supported)
  - Zone ID or zone name lookup
  - A and CNAME record support
  - Proxied/unproxied records with `PROXIED` setting
  - Rate limiting awareness
- **Webhook Provider**: Generic webhook for custom DNS integrations (#26)
  - Configurable endpoints for create/delete operations
  - Authentication via custom headers
  - Retry logic with configurable backoff
  - Enables integration with any DNS provider via HTTP API
- **TXT Record Ownership Tracking** (#37): Prevents orphan cleanup from deleting manually-created DNS records
  - Creates `_dnsweaver.{hostname}` TXT records with `heritage=dnsweaver` value
  - Only deletes records during orphan cleanup if ownership TXT record exists
  - Configurable via `DNSWEAVER_OWNERSHIP_TRACKING` (default: true)
  - All providers now support TXT records for ownership markers
- **Ownership State Recovery** (#40): Recover ownership state from DNS on startup
  - Scans all providers for `_dnsweaver.*` TXT records at startup
  - Repopulates known hostnames so orphan cleanup works after restarts
  - No manual intervention needed—dnsweaver remembers what it manages
- **Orphan Cleanup Configuration**: New `DNSWEAVER_CLEANUP_ORPHANS` setting (default: true)
- **Domain Exclusion**: `DNSWEAVER_<PROVIDER>_EXCLUDE_DOMAINS` for excluding domains from a provider

### Fixed
- **Cloudflare**: Return ErrConflict for duplicate records (error codes 81053, 81058)
- **Cloudflare**: Don't proxy TXT records (fixes error 9004)
- **Technitium**: Add required `domain` parameter when listing zone records
- **Reconciler**: Silence warnings when ownership TXT record already exists (expected case)

## [0.1.1] - 2026-01-07

### Added
- **TOML File Support**: Parse Traefik TOML configuration files in addition to YAML (#25)
  - Automatically detects file format by extension (`.toml`, `.yml`, `.yaml`)
  - Default file pattern now includes `*.toml` alongside YAML patterns
  - Mixed YAML/TOML directories fully supported

## [0.1.0] - 2026-01-07

### Added
- **Technitium DNS Provider**: Full implementation with create, update, delete operations
- **Traefik Source**: Extract hostnames from `traefik.http.routers.*.rule` Docker labels
- **Static File Discovery**: Parse Traefik dynamic configuration YAML files for Host rules
- **Multi-Provider Routing**: Route different domains to different DNS providers with glob/regex patterns
- **Split-Horizon DNS**: Support for internal and external records from the same container labels
- **Docker Swarm Support**: Full support for Docker Swarm services alongside standalone containers
- **Socket Proxy Support**: Connect via TCP to Docker socket proxy for improved security
- **Reconciliation Engine**: Periodic full sync ensures DNS records match running containers
- **Event-Driven Updates**: Real-time DNS updates on container start/stop events
- **Health Endpoints**: `/health`, `/ready`, and `/metrics` for monitoring and orchestration
- **Prometheus Metrics**: `dnsweaver_*` metrics for observability
- **Docker Secrets Support**: `_FILE` suffix for all sensitive environment variables
- **Multi-arch Images**: linux/amd64 and linux/arm64 Docker images

### Infrastructure
- Go module: `gitlab.bluewillows.net/root/dnsweaver`
- Minimum Go version: 1.23
- GitLab CI/CD pipeline with GitHub release automation
- Docker Hub and GitHub Container Registry publishing

[Unreleased]: https://github.com/maxfield-allison/dnsweaver/compare/v1.1.3...HEAD
[1.1.3]: https://github.com/maxfield-allison/dnsweaver/compare/v1.1.2...v1.1.3
[1.1.2]: https://github.com/maxfield-allison/dnsweaver/compare/v1.1.1...v1.1.2
[1.1.1]: https://github.com/maxfield-allison/dnsweaver/compare/v1.1.0...v1.1.1
[1.1.0]: https://github.com/maxfield-allison/dnsweaver/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/maxfield-allison/dnsweaver/compare/v0.9.3...v1.0.0
[0.9.3]: https://github.com/maxfield-allison/dnsweaver/compare/v0.9.2...v0.9.3
[0.9.2]: https://github.com/maxfield-allison/dnsweaver/compare/v0.9.1...v0.9.2
[0.9.1]: https://github.com/maxfield-allison/dnsweaver/compare/v0.9.0...v0.9.1
[0.9.0]: https://github.com/maxfield-allison/dnsweaver/compare/v0.8.1...v0.9.0
[0.8.1]: https://github.com/maxfield-allison/dnsweaver/compare/v0.8.0...v0.8.1
[0.8.0]: https://github.com/maxfield-allison/dnsweaver/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/maxfield-allison/dnsweaver/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/maxfield-allison/dnsweaver/compare/v0.5.3...v0.6.0
[0.5.3]: https://github.com/maxfield-allison/dnsweaver/compare/v0.5.2...v0.5.3
[0.5.2]: https://github.com/maxfield-allison/dnsweaver/compare/v0.5.1...v0.5.2
[0.5.1]: https://github.com/maxfield-allison/dnsweaver/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/maxfield-allison/dnsweaver/compare/v0.4.2...v0.5.0
[0.4.2]: https://github.com/maxfield-allison/dnsweaver/compare/v0.4.1...v0.4.2
[0.4.1]: https://github.com/maxfield-allison/dnsweaver/compare/v0.3.3...v0.4.1
[0.3.3]: https://github.com/maxfield-allison/dnsweaver/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/maxfield-allison/dnsweaver/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/maxfield-allison/dnsweaver/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/maxfield-allison/dnsweaver/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/maxfield-allison/dnsweaver/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/maxfield-allison/dnsweaver/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/maxfield-allison/dnsweaver/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/maxfield-allison/dnsweaver/releases/tag/v0.1.0
