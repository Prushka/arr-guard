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
- Resolve the complete authoritative episode mapping before remediation. Guard
  searches target only its still-missing episodes or a confirmed movie ID. Never
  issue a whole-series search.
  Arr-owned automatic recovery retains Arr's release/season search scope.
- Keep dry run free of API mutations and retry-state writes, including cleanup.

## Code map

- `config.go`: environment configuration and redacted logging.
- `arr.go`, `model.go`: v3 API requests and resource models.
- `probe.go`: ffprobe and matching subtitle sidecars; no media writes.
- `probe_diagnostics.go`: exact recoverable-video diagnostic classification and
  bounded diagnostic logging; unknown/subtitle/container failures remain protected.
- `service.go`: webhooks, library/unmatched scans, queue recovery, remediation.
- `queue_recovery.go`: rejected/partial download preflights and missing-target
  searches; remove client tasks and downloaded files while retaining library media.
- `state.go`: retry state and atomic JSON persistence.
- `safety.go`: identity, history/redownload, and filesystem preflight checks.
- `jobs.go`: durable webhook admission, deferred retries, and review pauses.
- `retry.go`: safe pre-mutation retry classification, backoff, and scan retries.
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
  import history before remediation, including when the webhook arrives before
  the history is visible. Probe first; already-valid files need no origin lookup.
  A temporarily unassigned Sonarr file waits for an authoritative mapping. A file
  disappearing during preflight gets a fresh read to settle the obsolete work.
- History remediation must work with automatic failed redownload enabled. Resolve
  whether Arr will search for the selected grabbed release, including the
  interactive-search exception; skip the guard's duplicate search when Arr owns
  replacement. Never change server settings. Keep unknown-config and uncertain
  mutation outcomes protected. MAX_ATTEMPTS limits guard-issued searches, not
  Arr's independent automatic recovery. Leave queued origins alone until Arr
  reports the whole download imported;
  queued Sonarr episodes must all have managed files.
- Serialize mutation sequences, persist their phase before each network write, and
  preserve ambiguous outcomes for manual reconciliation. Cleanup never replays a
  mutation. A disappearance or HTTP error does not establish transaction outcome.
- Serialize remediation preflights in dry run too. Concurrent history lookups can
  overload Arr and produce timeout refusals that do not occur in write mode.
  Keep webhook origin checks after probing and inside that serialization. Keep
  media probes parallel; preserve the normal HTTP deadline.
- Retry counters follow individual episodes/movie IDs; old composite episode keys
  migrate conservatively. Accepted write-enabled webhooks persist before HTTP 202.
- A valid probe may reset retry counters only while its media and
  matching-subtitle snapshots still match; a changed/missing snapshot preserves
  counters. Ignore unrelated directory mtime changes, retain directory identity.
- A successful ffprobe may continue past only the exact MPEG-2 error-level
  `Invalid frame dimensions 0x0.` diagnostic when all reported MPEG-2 video streams
  have positive dimensions. Keep severity/context, complete JSON, output bounds,
  and snapshots enforced. Other diagnostics still fail with their actual text.
  Never infer a subtitle language from the codec name. Disable FFREPORT and forced
  color in the ffprobe child process; probes must not create report files.
- Bind the HTTP listener before starting workers. Stop cancels in-flight probes
  and leaves accepted durable jobs available for the next start.
- State persistence failure disables subsequent state writes in that process. One
  writer holds STATE_PATH.lock; server identity is bound to its state file.

Native regression/integration tests, real ffprobe fixtures, race detection, vet,
lint, and Linux build/test compilation have passed. Live GET-only checks cover
both libraries, webhook processing, stale-snapshot refusal, queue reads, and the
combined unmatched scan, with zero API mutations or media writes. The largest
full pass attempted 10,813 probes (14 refused ffprobe error diagnostics) and
excluded 10 files by age. Six concurrent timeout cases passed after history
serialization using the normal 30-second request deadline. AUDIT.md records counts,
independent library changes, and remaining platform/verification limits. Never
infer completion from a started or interrupted test process.

The initial automatic-redownload follow-up passed native tests, race detection, vet,
lint (zero issues), and Linux/amd64 application/test compilation. Four previously
blocked live files (two per Arr server) passed GET-only remediation preflight and
webhook dry runs with Arr owning replacement: 92 GETs, zero mutations, retry-state
writes, or media changes. Local fixtures verify actual mutation sequences,
interactive-search exceptions, settings changes, journal failures, and no
duplicate/fallback searches. Legacy exhausted webhook jobs can resume fresh
evaluation after upgrading; the operation journal still prevents mutation replay.

Workflow recovery now compares only matching subtitle files, probes before
origin lookup, and searches remaining missing targets after remediation. Safe
pre-mutation read/probe/import failures retry indefinitely in write-enabled serve
mode with backoff capped at one hour. Permanent identity/configuration errors and
uncertain operations pause with `needsReview`; explicit redelivery allows fresh
evaluation, never mutation replay. Legacy jobs retain their backoff on upgrade.
One-time scans make up to three fresh attempts, then save unresolved safe per-file
work for a later write-enabled serve process using the same state. Dry run never
saves jobs. Scans return an error for unresolved work and do not become daemons.

Keep series-wide exclusion for uncertain operations: automatic Arr failure/search
effects can extend beyond a file's episode mapping. Never classify a mutation's
underlying network error as permission to replay it. Queue recovery may retire a
rejected partial download while retaining managed library media. Always request
client task and downloaded-file removal with `removeFromClient=true`. Resolve
all targets from current queue/history, refuse active or ambiguous shared items,
and search only missing targets. Actual guard search targets are
journaled in `searchEpisodeIds`; already-replaced targets do not block the missing
subset. A present replacement still needs its own subtitle validation.

Workflow recovery passed native tests, race detection, vet, lint (zero issues),
and Linux/amd64 application/test compilation. Legacy exhausted-job fixtures verify
upgrade recovery and preservation of uncertain-operation protection.
The full live GET/dry-run pass completed in 35 minutes 27 seconds: 10,823 accessible
files, 10,799 usable probes, 10,615 policy accepts, 184 policy rejections, 14 probe
errors, 10 age exclusions, and zero preflight blocks or size mismatches. Both real
webhook checks, stale-snapshot refusals, queue recovery dry run (seven import-blocked
Sonarr queue rows), and the combined unmatched scan passed. A final four-file
history/search-ownership and webhook retest passed with 88 GETs. All live tests
made zero API mutations, retry-state writes, or media changes. AUDIT.md separates
these read-only results from mutation behavior verified only by local fixtures.

Recoverable probe diagnostics passed native tests, race detection, vet, lint
(zero issues), and Linux/amd64 application/test compilation. The reported MPEG-2
file passed repeated real probes and both scan/webhook remediation preflights.
A targeted 19-file live pass made 125 GETs: five normal policy rejections reached
preflight, including that recovered file; 14 other probe failures remained
protected, and no successful probe was blocked at preflight. A separate library
compatibility pass checked 10,921 accessible files with no size mismatches, plus
sample valid probes, webhooks, stale-snapshot refusal, and queue reads on both
servers. These tests made zero API mutations, retry-state writes, or media changes.
This was targeted probe verification, not another full-library probe pass;
AUDIT.md records the results and the remaining Linux/runtime verification limits.

Partial/waiting-import recovery recognizes the six requested rejection families
in completed importPending rows, retains active-import checks, and does not change
DRY_RUN or RECOVER_BLOCKED_QUEUE defaults. Recovery also runs after each listed
library in an opted-in one-time subtitle scan. Current queue and grabbed/import
history resolve full targets; grabbed history is required because Arr otherwise
cannot reliably blocklist. Remove client tasks and downloaded files even when
current media or prior imports exist. Do not search or charge retries for
all-existing targets. Suppress searches covered by active downloads or already
requested in that queue scan.

This follow-up passed native tests, race detection, vet, lint (zero issues), and
Linux/amd64 application/test compilation. The live queue pass used 241 GETs:
24 Sonarr download plans (six partial, eight all-existing, 13 search plans) and
one Radarr plan. Five Sonarr downloads lacked grabbed history and stayed protected.
The exact reported duplicate packs passed a separate 19-GET dry run: preserve
episodes 05–06 and search 07–08 once. All live checks made zero API mutations,
retry-state writes, or media changes. They use empty retry state; deployment
counter/journal eligibility and actual server mutations remain unverified.

The September 11 correction removes the partial-import client-file retention
exception at the user's request. All queue removals request `removeFromClient=true`
alongside blocklisting and suppressed Arr redownload; imported library files remain
untouched and searches still target only missing media. Native tests, race detection,
vet, lint (zero issues), and Linux/amd64 application/test compilation passed.
The fresh live GET/dry-run pass used 425 GETs: 43 Sonarr plans (seven partial,
19 all-existing, 21 search plans) and two Radarr search plans. Five Sonarr downloads
still lacked grabbed history. No API mutations, retry-state writes, or media writes
occurred. Local HTTP fixtures verify removal flags, including prior imports with
present or missing media; actual client deletion and deployment remain untested.

Known shared-download limit: preflights inspect only the current Arr instance.
They do not coordinate another Sonarr/Radarr instance using the same client task.
A queue removal affects the entire client download and can interrupt another
consumer. Sonarr's episode queue rows inherit download-level state and diagnostics;
do not describe them as independent per-file readiness checks. Same-instance group
tests do not establish protection across instances or atomicity with Arr imports.
