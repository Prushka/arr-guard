# Safety audit — September 2026

The audit covers the Go application, tests, launch/lint/build scripts, Docker
configuration, and documentation. No production mutation, setting change, media
write, deletion, move, or rename is used for verification. `.env` is read privately
by an opt-in test harness; it is not changed. Mutation scenarios use local HTTP
fixtures and temporary test media.

## Findings and fixes

| Area | Risk found | Implemented behavior |
| --- | --- | --- |
| Configuration | Invalid `DRY_RUN` silently became false; unauthenticated write serving | Dry run defaults true; invalid settings fail; write serving requires authentication |
| API boundary | Redirects could forward the API key; successful empty/malformed responses could create zero-value resources | Redirects disabled; independent read-only client gate; resource identity and bounded JSON validation |
| Pagination | History stopped after 1,000 records; changing/repeating queue pages could misidentify work | Paginated history/queue reads with duplicate-ID, completeness, and page-limit checks |
| File identity | Webhook paths/IDs and stale scan records could direct deletion/search | Authoritative Arr ownership, path, file snapshot, current episode mappings, and origin checks |
| Probe safety | Changing/incomplete files, error diagnostics despite successful exit, and unbounded output could produce false rejection or resource exhaustion | Nonempty regular files, bounded output, error-diagnostic rejection, timeout/pipe cleanup, filesystem snapshots; probe errors leave media untouched |
| Retry accounting | Composite episode groups, inconsistent resets, or a disappearing sidecar after validation could bypass or exhaust caps | Individual episode/movie counters, conservative legacy migration, valid-file resets guarded by media and subtitle directory snapshots |
| State persistence | In-memory state changed on failed writes; multiple processes could overwrite state | Copy-on-write persistence, exclusive OS lock, flushed replacement, server binding, failure latch |
| Partial mutation | Process failure lost post-delete work; shutdown could repeat a committed action | Durable operation phase before every mutation; uncertain outcome requires reconciliation, never blind replay |
| Origin failure | Wrong history record or Arr automatic redownload could cause broad/duplicate searches | Confirmed grabbed history; both automatic failed-redownload flags must be off for history failure; queue uses skipRedownload |
| Shared downloads | One queue entry could remove an entire pack while searching only one episode | Group and recheck the entire download, include all affected queue episodes, protect active/partially imported/existing media |
| Queue recovery | Automatic destructive recovery had no retry budget | Explicit opt-in, shared retry caps, durable operation record, no success inference from disappearance |
| Webhook lifecycle | HTTP 202 could acknowledge work lost on shutdown; one failed file prevented later files in a batch | Persist before acknowledgement, process other files after an error, bounded retries, preserve accepted jobs on stop |
| Startup/scans | Recovery could start before HTTP bind succeeded; one failed Arr scan prevented the other | Bind first; bound scan concurrency and continue other configured instances |
| History read concurrency | Concurrent dry-run and early webhook history reads caused six live request timeouts | Serialize expensive preflights and origin checks in both modes, retain parallel probes and the normal request deadline |
| Paths/reports | UNC/case mapping bugs, Unicode byte-slicing panics, root aliases, and colliding report/state paths | Preserve mapped suffixes and UNC paths; normalize subtitle slicing; resolve scan-root aliases; reject output collisions and output inside mapped media |
| Defaults/docs | Example restarted a one-shot scan and documented broader language grace than implemented | Safe example defaults; documented actual unidentified-language grace and explicit recovery limitations |

The 50-year age exclusion remains a user-visible policy. It is not evidence that
all media older than 50 years is silent. Unknown contained episode dates prevent
an age exclusion rather than borrowing the series premiere date.

## Verification

Final native unit/integration tests, the Go race detector, `go vet`, and pinned
golangci-lint passed; lint reported zero issues. Real ffmpeg/ffprobe fixtures cover
English, non-English, unidentified, absent, external, and unrelated subtitles.
Fault-injection tests cover stale media and subtitle directories, partial API
failures, persistence failures, duplicate deliveries, restart, cancellation,
retry migration/caps, and concurrent webhook/preflight reads.

Linux/amd64 application and test binaries cross-compile. Linux runtime tests are
unavailable on this host: Docker is not installed and WSL has no runtime.
PowerShell launch-script and Bash build-script syntax checks passed; deployment
and image publishing were not executed.

Three complete full-library passes ran during the audit. The most recent finished
in **37 minutes 32 seconds**, with these results:

| Instance | Managed files | Usable probes | Policy accepted | Policy rejected | Probe errors | Age excluded |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Sonarr | 10,265 | 10,253 | 10,162 | 91 | 12 | 0 |
| Radarr | 420 | 408 | 406 | 2 | 2 | 10 |
| Total | 10,685 | 10,661 | 10,568 | 93 | 14 | 10 |

All managed files were accessible with no file-size mismatches. There were
**zero server mutations or media writes**. The combined unmatched scan found
2,052 files beneath one mapped root; its report used a temporary local directory.

The 14 probe errors are ffprobe error diagnostics, not inaccessible paths. Those
files were left untouched and cannot be counted as successfully validated.
The 93 policy rejections are subtitle-policy results, not a claim that those
files are corrupt.

That full pass protected 84 candidate remediations: 78 at the automatic
failed-redownload setting check and six at the HTTP deadline. Detailed diagnostics
reproduced all six deadline failures and identified Sonarr's
`GET /api/v3/history/series` as the slow read (13 seconds in an isolated trace).

The final concurrency fix serializes remediation preflights in both modes and
early webhook origin-history checks, while retaining parallel media probes.
Regression tests first reproduced eight overlapping reads, then passed with a
peak of one. All six affected files were submitted concurrently against the live
server after the fix: the batch completed in **2 minutes 5 seconds** with the
normal **30-second per-request deadline**, zero timeouts, and zero mutations.
Each reached the expected automatic-redownload safety check. Waiting for the
preflight lock is separate from an individual HTTP request's deadline.

Real webhook processing and deliberate stale-directory snapshot refusals passed
on both servers; only test memory was altered to inject the stale snapshot.
The final compatibility/webhook pass after the early webhook history fix passed
in 1 minute 23 seconds. It inventoried 10,290 Sonarr files and 420 Radarr files,
all accessible with matching sizes, and made 1,383 GET requests with zero
mutations. New imports explain the increase since the full-scan snapshot above.

Read-only diagnostics also inspected Sonarr import histories and both queues.
They found no current ambiguous import histories or fully imported queue groups;
no import-blocked queue items were present. Queue removal/recovery failures were
therefore exercised with local fixtures. Earlier full passes completed in
35 minutes 56 seconds and 37 minutes 22 seconds; counts changed as the library
continued importing independently. The latest table supersedes earlier counts.

| Functionality | Local verification | Live read-only verification |
| --- | --- | --- |
| Library/subtitle decisions | Language, age, empty/changing files, external subtitles, real ffprobe fixtures | Both complete libraries, mapped file access/size checks, real probes and remediation preflight |
| Webhooks | Authentication, payload validation, duplicate/concurrent delivery, durable acknowledgement, retries and shutdown | A real file's webhook processing in dry run on each server |
| Queue recovery | Shared/active packs, existing replacements, caps, failed/disappeared removals | Paginated queue reads and recovery dry run; no import-blocked items were present |
| Delete, blocklist, search | Local HTTP fixtures, partial failures, stale identities, persistence faults, cancellation and restart | Preflight reads only; production mutations deliberately excluded |
| Retry state | Migration, corrupt input, write rollback, exclusive lock, server binding, uncertain-operation retention | Dry run confirmed no retry-state changes |
| Unmatched scan | Extensions, exclusions, root aliases, managed-file matching, temporary report | Combined scan of the mapped root; temporary local report only |
| Startup/deployment | Serve health, cancellation, bind failure, mode dispatch, script syntax, Linux cross-compilation | Status/API compatibility; no normal production startup or container run |

## Limits and operational changes

Production mutation behavior is simulated, not executed. Live tests add an
independent GET-only transport with exact origin/path restrictions and disabled
redirects; they exercise reads, real media probes, and dry-run decisions only.
Reports contain counts, numeric file IDs, and error categories, not credentials,
private URLs, or media paths. A passing harness means it completed safely;
per-file probe and preflight refusals remain explicit verification limitations.

The public Arr APIs provide no atomic multi-request transaction or conditional
delete. Rechecks cannot eliminate races with an external importer in the final
check-to-request interval. The guard therefore records uncertain operations and
requires deliberate reconciliation; see README.md. Only one write-enabled guard
should manage an Arr server with the same state path. Independent state files and
database restores require operator coordination.

`DRY_RUN=true` and `RECOVER_BLOCKED_QUEUE=false` are now defaults. Write serving
requires authentication. History failure will be refused while either Arr
automatic failed-redownload setting is enabled or unknown. The guard never
changes those server settings itself. Fully imported queued downloads remain
eligible; incompletely imported packs are retained.

API behavior was checked against the upstream
[Sonarr history controller](https://github.com/Sonarr/Sonarr/blob/main/src/Sonarr.Api.V3/History/HistoryController.cs),
[Radarr history controller](https://github.com/Radarr/Radarr/blob/master/src/Radarr.Api.V3/History/HistoryController.cs),
[Radarr queue controller](https://github.com/Radarr/Radarr/blob/master/src/Radarr.Api.V3/Queue/QueueController.cs),
and [Sonarr completed-download service](https://github.com/Sonarr/Sonarr/blob/main/src/NzbDrone.Core/Download/CompletedDownloadService.cs).
