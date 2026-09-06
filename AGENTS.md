# Arr Guard contributor instructions

Keep this file and README.md consistent with code changes. This repository is a
single-package Go 1.26 application using the standard library, with PowerShell
launch/lint scripts and Docker deployment files. No generated API client is used.

## Safety boundaries

- Never use production Sonarr/Radarr mutation endpoints for testing. Live tests
  must force dry run AND enforce a GET-only HTTP transport with redirects disabled.
  Never write, delete, rename, move, or edit server media or download files.
- `.env` contains private credentials. Read it without printing its values; do not
  commit it or include credentials, private URLs, or media paths in test reports.
- Test mutation workflows against local `httptest` servers and temporary fixtures.
  Do not run `serve.ps1`, Docker Compose, or a normal application invocation against
  `.env` while auditing: they may enable destructive behavior.
- Arr owns all media mutations. The sidecar only reads local media. Failed probes,
  ambiguous identities, invalid configuration, and state persistence failures must
  stop remediation. Do not treat a disappeared queue item as proof of blocklisting.
- Searches must target confirmed movie IDs or the complete authoritative episode
  mapping for a file. Never fall back to a whole-series search.
- Keep dry run free of API mutations and retry-state writes, including cleanup.

## Code map

- `config.go`: environment configuration and redacted logging.
- `arr.go`, `model.go`: v3 API requests and resource models.
- `probe.go`: ffprobe and matching subtitle sidecars; no media writes.
- `service.go`: webhooks, library/unmatched scans, queue recovery, remediation.
- `state.go`: retry state and atomic JSON persistence.
- `safety.go`: identity, history/redownload, and filesystem preflight checks.
- `jobs.go`: durable webhook admission and bounded retries of failed jobs.
- `lock_windows.go`, `lock_unix.go`: exclusive state-writer locks.
- `main.go`: startup, signal handling, and shutdown.
- `*_test.go`: unit and local HTTP integration tests; live checks must be opt-in.

## Verification

Run `go test ./...`, `go test -race ./...`, `go vet ./...`, and `./lint.ps1` after
safety-related changes. Use fault injection, stale snapshots, duplicate deliveries,
concurrency, restart, and cancellation tests where those behaviors change. Live
GET/dry-run checks validate compatibility; they cannot prove server mutation
behavior. Record that distinction and any inaccessible media in the audit report.
Inspect live per-file failure categories and counters even when the harness
passes; completion does not mean every probe or remediation preflight succeeded.

## Implemented audit invariants

- Dry run defaults to true; malformed booleans/integers fail startup. Write-enabled
  serving requires authentication. Queue recovery requires RECOVER_BLOCKED_QUEUE.
- Validate current API ownership, complete episode mappings, and probe snapshots
  before deletion. Webhook paths, download IDs, dates, and episode IDs are hints
  only; use Arr resources as authority. A provided webhook download ID must match
  import history, including when the webhook arrives before the history is visible.
- Preflight history failure requires both automatic failed-redownload settings off.
  Leave queued origins alone until Arr reports the whole download imported;
  queued Sonarr episodes must all have managed files.
- Serialize mutation sequences, persist their phase before each network write, and
  preserve ambiguous outcomes for manual reconciliation. Cleanup never replays a
  mutation. A disappearance or HTTP error does not establish transaction outcome.
- Serialize remediation preflights in dry run too. Concurrent history lookups can
  overload Arr and produce timeout refusals that do not occur in write mode.
  Include early webhook origin checks in that serialization. Keep media probes
  parallel; preserve the normal HTTP deadline.
- Retry counters follow individual episodes/movie IDs; old composite episode keys
  migrate conservatively. Accepted write-enabled webhooks persist before HTTP 202.
- A valid probe may reset retry counters only while its media and subtitle
  directory snapshots still match; a changed/missing snapshot preserves counters.
- Bind the HTTP listener before starting workers. Stop cancels in-flight probes
  and leaves accepted durable jobs available for the next start.
- State persistence failure disables subsequent state writes in that process. One
  writer holds STATE_PATH.lock; server identity is bound to its state file.

Native regression/integration tests, real ffprobe fixtures, race detection, vet,
lint, and Linux build/test compilation have passed. Live GET-only checks cover
both libraries, webhook processing, stale-snapshot refusal, queue reads, and the
combined unmatched scan, with zero API mutations or media writes. The largest
full pass attempted 10,675 probes (14 refused ffprobe error diagnostics) and
excluded 10 files by age. Six concurrent timeout cases passed after history
serialization using the normal 30-second request deadline. AUDIT.md records counts,
independent library changes, and remaining platform/verification limits. Never
infer completion from a started or interrupted test process.
