# Safety audit — September 2026

The audit covers the Go application, tests, launch/lint/build scripts, Docker
configuration, and documentation. No production mutation, setting change, media
write, deletion, move, or rename is used for verification. `.env` is read privately
by an opt-in test harness; it is not changed. Mutation scenarios use local HTTP
fixtures and temporary test media.

## GitHub CI/CD follow-up — September 13

Added `.github/workflows/docker.yml` for branch pushes, pull requests, and manual
runs. Go formatting/race/vet/lint and workflow checks precede the existing
linux/amd64 Docker build. The image is smoke-tested with no network, no media or
configuration mounts, a read-only filesystem, and the expected non-root UID before
publishing that same image. Pull requests cannot publish; missing Docker Hub
secrets skip publishing without disabling builds. Actions are pinned to verified
release commits and the GitHub token is restricted to read-only repository access.

Each eligible push publishes its seven-character commit tag. A separate serialized
job promotes the digest to `latest` only while its commit remains the default-branch
head, preventing a slower stale build from overwriting a newer promotion. CI never
receives Arr secrets or live-test opt-ins. `.github` is excluded from the Docker
build context; `.env` and private artifact exclusions remain intact. `docs/CI.md`
documents the two required repository secrets and first-run instructions.

Local Go formatting, uncached full race tests, vet, and pinned lint passed. Lint
reported zero issues. Actionlint v1.7.12 with ShellCheck v0.11.0 and ten inline Bash
syntax checks passed. Extracted workflow scripts passed mocked tests for all four
credential-presence combinations, successful commit publishing, smoke/push failures,
invalid image digests, successful current-head promotion, stale-head refusal, and
failed GitHub reads. These tests used fake credentials and no registry writes.
Validation tools/scripts/output are isolated under ignored `bin/` and `logs/`.

Docker is unavailable on this host, so actual image build/execution, Linux CI
execution, GitHub scheduling, and Docker Hub publishing remain unverified locally.
The workflow performs its build and container smoke tests on GitHub even before
secrets are added. No production Arr request, configuration/media change, image
push, or deployment was performed for this follow-up.

## Imported-file attempt limit correction — September 12

The imported-file budget now gates the complete remediation sequence. Once any
movie/authoritatively mapped episode counter reaches `MAX_ATTEMPTS`, Guard logs a
policy skip before origin lookup, deletion, queue removal, failure/blocklisting,
or search. It neither increments the counter nor creates an operation record.
This applies in scan and serve, including histories that would cause Arr-owned
automatic redownload. The final allowed attempt still completes; an already
exhausted or previously over-limit counter preserves subsequent rejected files.
A valid file can still reset its counters under the existing snapshot rules.
Uncertain operations retain their journal protection; this change does not replay
or undo previous mutations. It does not cancel searches already started by Arr.
The conditional waiting-import fallback remains a separate workflow.

Native tests, race detection, `go vet`, lint (zero issues), and Linux/amd64
application/test compilation passed. Local fixtures cover at-limit and over-limit
files in full scans, dry runs, and concurrent webhooks for both Arr applications:
automatic/interactive recovery, Guard-owned searches, queued origins, absent or
unavailable history, and already-failed origins. They assert no API mutations,
media changes, counter changes, new operations, or origin reads. Boundary tests
verify the last permitted attempt completes and a new file ID after restart cannot
evade the persisted budget. A single exhausted episode protects a combined file
without charging its other episodes; valid snapshot-checked files can still reset
their counters.

Live GET-only verification made **125 GETs**: 29 for real probes, ordinary
scan/webhook checks, and the new limit checks; 96 for the existing ten queue
recovery plans. Both previously reported English-subtitle examples remained
accepted, and the identified PGS subtitle error remained protected. For the cap
checks, isolated test memory simulated exhausted budgets and rejected validation
while retaining current Arr ownership and real media snapshots. Both scan and
webhook paths logged policy skips with no state changes. These injected checks do
not claim that the two live files lack subtitles or that production counters were
exhausted. All live requests were GET-only; no production API mutations, runtime
state/configuration writes, or media writes occurred. Neither deployment nor Linux
runtime execution was performed. Logs are under ignored `logs/`.

## Exhausted matched-by-ID import follow-up — September 12

Completed waiting-import downloads can now use Arr's manual-import workflow after
every selected missing movie/episode has exhausted `MAX_ATTEMPTS`. Only the movie
or series "Found matching ... via grab history, but release was matched ... by ID"
reason qualifies, with no additional rejection. The existing
`RECOVER_BLOCKED_QUEUE` gate applies in scan and serve; no application environment
variables or defaults were added or changed.

Guard discovers files with `GET /api/v3/manualimport` and submits selected files
through `POST /api/v3/command` with `name: "ManualImport"` and `importMode: "auto"`.
It never copies, moves, edits, or deletes media through the filesystem. Arr owns
import and normal download cleanup. `POST /manualimport` reprocesses selection
metadata; it is not the command used to perform the import. These distinctions
were checked against upstream
[Sonarr ManualImportService](https://github.com/Sonarr/Sonarr/blob/develop/src/NzbDrone.Core/MediaFiles/EpisodeImport/Manual/ManualImportService.cs)
and [Radarr ManualImportService](https://github.com/Radarr/Radarr/blob/develop/src/NzbDrone.Core/MediaFiles/MovieImport/Manual/ManualImportService.cs).

Each selected filename must independently identify the intended movie or exact
episodes through Arr's parse API or a unique normalized catalog title/alias match.
Fallback movie matching requires the year. Episode numbers must resolve uniquely
using native/scene seasonal or absolute numbering; grab history alone does not
establish content identity. Selected files must pass the existing subtitle policy,
cover all missing targets exactly once, preserve Arr's quality/language metadata,
and remain within the download source tree, outside the library. Existing files
are not selected for replacement. Fresh decisions, queue/history ownership,
target absence, source identity, and subtitle snapshots are required after probes.
Other configured Arr queues are checked for shared client tasks/output paths.

Submission is journaled before the API write without charging or resetting retry
counters. Pending imports are reconciled at startup, on subsequent queue scans,
and every ten seconds in serve. New matching import history, correct current
library assignments, and passing library probes establish success; an accepted or
missing command alone does not. Lost acknowledgements may be resolved from those
reads, but an uncertain import is never submitted again automatically.

A partial import may leave a rejected queue task because Arr counts only files
imported by the current command. Once the selected files are verified and every
pack target has a managed file, Guard can remove that residual task through Arr's
queue API. It rechecks ownership, output path, target presence, and other configured
consumers, then journals cleanup before `DELETE`. Flags request client task and
downloaded-file removal, with no blocklist or redownload. An uncertain cleanup is
never replayed. Dry-run, read-only client, and unmatched mode prevent these writes;
turning off queue recovery prevents new residual cleanup.

Final `go test ./...`, `go test -race ./...`, `go vet ./...`, and `./lint.ps1` passed;
lint reported zero issues. Linux/amd64 application and guard-test binaries
cross-compiled; they were not executed on Linux. Local HTTP fixtures exercise
successful movie/episode imports, partial packs, exact request metadata and cleanup
flags, shared consumers, mixed budgets, duplicate/concurrent delivery, stale
files/sidecars/queue decisions, identity conflicts, persistence failures, lost
acknowledgements, failed/late commands, restart, and read-only state preservation.
All media writes and removals in those fixtures are disposable simulations of Arr.

Live verification used independent GET-only, exact-origin transports with redirects
disabled and forced dry run. The user-provided completed-download roots were added
only to the test's in-memory mappings. Exhausted retry counts were also simulated
in memory; deployment counters and configuration were not changed.

| Live check | Result | GETs |
| --- | --- | ---: |
| Final-import preview | Nine downloads: eight Sonarr, one Radarr; none approved | 126 |
| Existing queue recovery below the cap | Nine Sonarr and one Radarr plan, no preflight refusals | 96 |
| Existing probe/webhook regression | Two English-subtitle accepts; one identified PGS subtitle error remained protected | 18 |

The nine final-import refusals were four unknown/ambiguous series titles, one
unresolved/conflicting episode mapping, and four source subtitle-policy failures
(three Sonarr, one Radarr). The reported Doraemon download reached source probing
but still failed subtitle validation, so this policy does not force it into the
library. The previews tested compatibility and decisions, not actual production
import or cleanup. All live checks made zero API mutations, runtime-state writes,
configuration changes, or media writes. Neither `.env` nor the deployment
configuration directory was modified. Logs remain under ignored `logs/`.

Remaining limits: unknown identities and invalid subtitles stay protected; an
unconfigured Arr consumer cannot be coordinated. Arr offers no atomic transaction
across preflight and command submission, so external-import races cannot be fully
eliminated. Unverifiable command/cleanup outcomes remain journaled. This fallback
did not change imported-file remediation; the attempt-limit correction above
subsequently added that gate. Arr's independently initiated replacement searches
remain outside Guard's control. Positive server mutation behavior was verified only with
local fixtures; no deployment was performed.

## Findings and fixes

| Area | Risk found | Implemented behavior |
| --- | --- | --- |
| Configuration | Invalid `DRY_RUN` silently became false; unauthenticated write serving | Dry run defaults true; invalid settings fail; write serving requires authentication |
| API boundary | Redirects could forward the API key; successful empty/malformed responses could create zero-value resources | Redirects disabled; independent read-only client gate; resource identity and bounded JSON validation |
| Pagination | History stopped after 1,000 records; changing/repeating queue pages could misidentify work | Paginated history/queue reads with duplicate-ID, completeness, and page-limit checks |
| File identity | Webhook paths/IDs and stale scan records could direct deletion/search | Authoritative Arr ownership, path, file snapshot, current episode mappings, and origin checks |
| Probe safety | Changing/incomplete files, error diagnostics despite successful exit, and unbounded output could produce false rejection or resource exhaustion | Nonempty regular files, bounded output, subtitle-relevant diagnostic classification, actual failure diagnostics, timeout/pipe cleanup, filesystem snapshots; failed probes leave media untouched |
| Retry accounting | Composite episode groups, inconsistent resets, or a disappearing sidecar after validation could bypass or exhaust caps | Individual episode/movie counters, conservative legacy migration, valid-file resets guarded by media and matching-subtitle snapshots |
| State persistence | In-memory state changed on failed writes; multiple processes could overwrite state | Copy-on-write persistence, exclusive OS lock, flushed replacement, server binding, failure latch |
| Partial mutation | Process failure lost post-delete work; shutdown could repeat a committed action | Durable operation phase before every mutation; uncertain outcome requires reconciliation, never blind replay |
| Origin failure | Wrong history record or Arr automatic redownload could cause broad/duplicate searches | Confirmed grabbed history; delegate replacement to Arr when its effective automatic policy applies, otherwise issue a scoped guard search; queue uses skipRedownload |
| Shared downloads | One queue entry could remove an entire pack while searching only one episode | Resolve queue/history scope, recheck the group, refuse active/ambiguous items, retain imported library files, and search only missing targets; queue removal also requests client task/file deletion |
| Queue recovery | Automatic destructive recovery had no retry budget; pending rejections and partial imports could remain stuck | Explicit opt-in, selected pending-rejection recovery, missing-target retry caps, durable operation record, no success inference from disappearance |
| Webhook lifecycle | HTTP 202 could acknowledge work lost on shutdown; one failed file prevented later files in a batch | Persist before acknowledgement, process other files after an error, safe deferred retries with capped backoff, preserve accepted jobs on stop |
| Startup/scans | Recovery could start before HTTP bind succeeded; one failed Arr scan prevented the other | Bind first; bound scan concurrency and continue other configured instances |
| History read concurrency | Concurrent dry-run and early webhook history reads caused six live request timeouts | Serialize expensive preflights and origin checks in both modes, retain parallel probes and the normal request deadline |
| Paths/reports | UNC/case mapping bugs, Unicode byte-slicing panics, root aliases, and colliding report/state paths | Preserve mapped suffixes and UNC paths; normalize subtitle slicing; resolve scan-root aliases; reject output collisions and output inside mapped media |
| Defaults/docs | Example restarted a one-shot scan and documented broader language grace than implemented | Safe example defaults; documented actual unidentified-language grace and explicit recovery limitations |

The 50-year age exclusion remains a user-visible policy. It is not evidence that
all media older than 50 years is silent. Unknown contained episode dates prevent
an age exclusion rather than borrowing the series premiere date.

## Automatic redownload follow-up

The original audit blocked history-based remediation while either automatic
failed-redownload setting was enabled. That restriction has been removed at the
user's request. Remediation now completes with automatic recovery enabled:
delete the rejected file, mark the confirmed grabbed release failed, and skip
the guard's duplicate search when Arr is configured to search for that release.
Interactive-search grabs use both the master and interactive settings; other
grabs use the master setting. Queue removal still suppresses Arr's automatic
search and uses the guard's scoped search. An already-failed origin emits no new
failure event, so its missing file still needs a guard search.

The effective policy is read before deletion and rechecked immediately before
history failure. The journal records `automaticSearch` before the request, and
uncertain outcomes never cause a fallback search or blind replay. Unknown
required settings still stop remediation. Server settings are never changed.
That follow-up originally bounded only guard-issued searches and continued
history failure at the cap. The imported-file attempt-limit correction above now
skips the entire sequence at the cap, preventing new Guard-triggered automatic
searches. It cannot suppress searches Arr already started or initiates independently.
Arr can search a shared release's episodes or an entire season. External changes
between the final configuration read and the failure request remain a race that
the API cannot make atomic.

Local regression tests cover both Arr applications, release-source and setting
combinations, settings changing during deletion, missing configuration, queue and
already-failed origins, the retry cap, duplicate concurrent webhooks, persistence
failure before and after history failure, uncertain-request recovery, and dry-run
state/media preservation. `go test ./...`, `go test -race ./...`, `go vet ./...`,
and `./lint.ps1` passed after the final changes; lint reported zero issues.
Linux/amd64 application and test compilation passed as well. No deployment or
production mutation was performed.

Live GET-only retests of four previously blocked files (two per Arr application)
passed actual probes, remediation preflight, and webhook processing with
`arr_automatic_search=true`. They made **92 GET requests**, with zero mutations,
retry-state writes, or media changes, and completed in **5.81 seconds**. Actual
deletion/blocklisting/search behavior remains verified only by local fixtures.

## Workflow recovery follow-up

Five local reproductions confirmed that additional audit-added checks could stop
legitimate work. Four have been corrected; the series-wide exclusion remains
because safely narrowing it requires more certainty about shared-release effects.
All mutation reproductions and regression tests use local HTTP fixtures and
disposable media only.

| Regression | Current behavior | Protection retained |
| --- | --- | --- |
| Unrelated directory activity invalidates a probe | Compare the matching subtitle filenames and file identity/size/mtime, ignoring directory mtime | Media and directory identity still checked; added, removed, renamed, changed, or incomplete matching sidecars require a new probe |
| Unavailable history blocks a webhook before probing | Probe first; accept valid files without history. Rejected files with a webhook download ID wait and retry when matching history is unavailable | A conflicting origin still requires review; unavailable history cannot authorize deletion or blocklisting |
| Any unfinished operation blocks the same Sonarr series | **Retained.** Other series and valid-file checks continue | An uncertain history failure or automatic search can affect a release/season beyond the file's mapped episodes; episode-only exclusion could overlap it |
| One replacement aborts the entire remaining search | Re-read authoritative targets after remediation; search only missing episodes, or finish without searching when all targets have files | Unknown target identities still require reconciliation; partial-queue recovery retains imported library files while removing rejected client downloads |
| Five temporary webhook failures permanently exhaust work | Safe failures keep retrying with backoff capped at one hour; restart preserves them; legacy five-failure jobs resume fresh checks | Permanent identity/configuration errors and uncertain mutations pause with `needsReview`; redelivery rechecks but cannot replay a journaled mutation |

Temporary network/API failures, failed probes, changed snapshots, delayed history,
unassigned episode files, and still-importing origins are eligible for fresh checks
before any mutation. A media-resource 404 during preflight triggers a fresh read;
if the file is gone, its obsolete scan/job can finish without mutations.
The delays are 1, 2, 4, 8, 16, and 32 minutes, then one hour. Failure counts saturate
at seven for backoff; they are separate from remediation/search attempt counters.
HTTP authentication errors, mismatched ownership/origins, missing required
configuration, and uncertain journal entries require attention. After a mutation
has begun, even a read timeout is treated as reconciliation work, never as a reason
to repeat deletion, origin failure, or search.

One-time scans make up to three attempts for temporary list/per-file failures.
Per-file retries refresh metadata and probe again, bypassing webhook deduplication
while retaining the operation journal. After one- and two-second waits, unresolved
safe per-file work is persisted for later write-enabled serve processing with the
same state file. The scan continues other files/servers and returns an error for
unresolved work. Dry run never writes jobs. A failed startup or library listing
cannot create per-file jobs; it still needs a later restart/scan. Serve mode does
not add periodic full-library scans.

Search narrowing records the actual `searchEpisodeIds` before the request. An
uncertain search preserves those IDs for review. Detecting a replacement file does
not validate its subtitles; a new import webhook or later scan does that. Arr-owned
automatic searches retain Arr's scope and remain outside the guard's attempt cap.

Native tests, the race detector, vet, lint, and Linux/amd64 application/test
compilation passed after these changes; lint reported zero issues. New regression
tests exercise unrelated directory writes,
matching-sidecar changes despite restored directory timestamps, delayed origin
history, partial/all replacements, shared queue protections, fresh scan retries,
scan-to-serve recovery, more than five failures across restart, legacy exhausted-job
upgrade, review-job redelivery, truncated-read/timeout retry classification,
disappearing files, delayed episode assignment, mutation-failure refusal, and
retained series exclusion.

The full GET-only/dry-run pass completed in **35 minutes 27 seconds**:

| Instance | Managed files | Usable probes | Policy accepted | Policy rejected | Probe errors | Age excluded | Preflight blocks |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Sonarr | 10,402 | 10,390 | 10,209 | 181 | 12 | 0 | 0 |
| Radarr | 421 | 409 | 406 | 3 | 2 | 10 | 0 |
| Total | 10,823 | 10,799 | 10,615 | 184 | 14 | 10 | 0 |

All files were accessible with matching sizes. The 184 policy rejections reached
remediation preflight without a blocker; they remain on the server because the
test is read-only. The 14 ffprobe diagnostic failures were left unvalidated and
untouched. The ten age exclusions were not probed. These counts describe this
library snapshot; imports and other Arr activity continue independently.

Real webhook processing and deliberately stale in-memory snapshot refusals passed
on both servers. Queue dry-run checks passed with 721 Sonarr queue rows (seven
import-blocked) and 14 Radarr rows (none import-blocked). Queue groups were only
read and checked, never removed. The combined unmatched scan found 2,052 files
beneath one mapped root and wrote its report to a disposable local directory.
All live calls used forced dry run, an independent GET-only transport with
redirects disabled, and no retry-state or server-media writes.

The final targeted retest of four previously blocked files (two per server)
passed actual probes, history preflight, and Download/ImportComplete webhook
processing with Arr owning replacement. It completed in **3.75 seconds**, using
**88 GET requests**, with zero mutations or retry-state writes. Actual deletion,
blocklisting, search, recovery after API faults, and partial-mutation behavior
remain verified by local fixtures only. No deployment was performed.

Other intentional compatibility changes also need explicit operator awareness:
queue recovery now defaults off, dry run defaults on, write serving requires
authentication, and state/report paths and server bindings are stricter.
Unknown ffprobe error output or an empty/nonregular matching sidecar still stops
validation, even when a separate English subtitle source might suffice. The
recoverable video exception below narrows only the diagnosed MPEG-2 startup case.

These remaining restrictions may leave some media unresolved. Preserving known
state takes precedence over deleting a file with an inconclusive probe or issuing
unbounded replacement grabs. Reconciliation instructions are in README.md.

## Recoverable probe diagnostics — September 9

This section records the original narrow exception. The September 12 subtitle-only
diagnostics follow-up below supersedes its MPEG-2-only restriction.

The original blanket stderr check treated any ffprobe error diagnostic as a
failed subtitle probe and discarded its text on exit status zero. A reported
transport stream reproduced `[mpeg2video] Invalid frame dimensions 0x0.` while
ffprobe exited successfully, reported 1440×1080 video, and detected an ARIB caption
stream with no language tag. A larger probe budget reproduced the same findings.
No matching subtitle sidecar was present. This is evidence of recovered video
metadata, not proof that the captions are English or that the entire media file
is free of corruption.

The new rule requests compact metadata for all streams and permits only that
exact error-level MPEG-2 diagnostic when ffprobe exits successfully, its JSON has
a stream array, and every reported MPEG-2 video stream has positive dimensions.
Each diagnostic line is checked with its codec context and severity; any other
error still blocks validation. Subtitle classification uses only subtitle streams
and matching sidecars. It does not infer language from `arib_caption` or any other
codec name. Unknown-language age grace and remediation/search caps are unchanged.

Allowed diagnostics are retained in `probeWarnings` and logged as warnings.
Failures include their actual diagnostic text, deduplicated, without memory
addresses, and capped at 2,048 characters for logging. Classification uses the full
captured stderr before logging truncation. Existing stdout/stderr limits, exit
status checks, timeout/cancellation, and media/sidecar snapshots remain enforced.
The ffprobe child disables report and forced-color environment settings, so
`FFREPORT` cannot cause hidden report writes and ANSI coloring cannot change the
classification. The requested `repeat+level+error` logging supplies severity on
each message and avoids suppressed repeat summaries.

The exception is based on the observed file and the
[FFmpeg 7.1.1 MPEG decoder](https://github.com/FFmpeg/FFmpeg/blob/n7.1.1/libavcodec/mpeg12dec.c),
which logs that error before dimensions are available. The
[ffprobe documentation](https://ffmpeg.org/ffprobe.html) distinguishes error-level
diagnostics, which can be recoverable, from process failure. Other video, audio,
subtitle, and container diagnostics remain protected until separately understood
and tested; exit status zero alone is insufficient to bypass them.

Local tests cover English/non-English/absent/ARIB subtitles, matching and unrelated
sidecars, English audio metadata, positive/zero/missing/multiple video dimensions,
wrong context/severity, mixed and unknown errors, nonzero exit, malformed JSON,
late errors after repeated allowed lines, output limits, stale sidecars, log
formatting, actual local remediation/reset paths for both Arr applications, and
real ffprobe fixtures with implicit reporting disabled. `go test ./...`,
`go test -race ./...`, `go vet ./...`, and `./lint.ps1` passed; lint reported zero
issues. Linux/amd64 application and test binaries compiled successfully.

Completed live checks used forced dry run, independently enforced GET-only HTTP
transports, and disabled redirects:

- The exact reported Sonarr file was resolved using GETs and tested twice, then
  exercised through scan and webhook remediation preflight: 23 GETs in 31.55
  seconds. The known video diagnostic became a warning. Unidentified ARIB
  captions remained a normal subtitle-policy rejection; no English language was
  inferred and no replacement was actually requested.
- A final 19-file diagnostic pass completed in 16.02 seconds with 125 GETs.
  Five files reached normal policy rejection and both preflights, including the
  recovered example. The other 14 reproduced their protected probe failures.
  Repeated probes agreed, and successful probes had zero preflight blocks.
  An earlier candidate-only run failed its reported-file membership expectation;
  GET lookup identified the actual file before these successful retests.
- A separate compatibility pass completed in 194.21 seconds: 10,499 Sonarr and
  422 Radarr managed files were accessible, with zero size mismatches. Both
  servers passed a real valid-file probe/webhook dry run and stale-directory
  snapshot refusal using only an injected in-memory snapshot. Queue reads found
  221 Sonarr rows (35 import-blocked) and 17 Radarr rows (two import-blocked).
  The pass made 977 Sonarr and 482 Radarr GETs; those queue counts describe
  existing server state, not successful queue remediation.

All live checks made **zero API mutations, retry-state writes, or media changes**.
This follow-up did not repeat the earlier full-library subtitle scan. Mutation
sequences were verified only against local HTTP fixtures. Actual probes used the
configured Windows ffprobe build (`2026-05-28-git-7b46c6a2a3`); Linux runtime and
Docker execution were not exercised, and no deployment was performed. Unknown
diagnostics still defer work for retry and require separate diagnosis if they
persist; the new exception is deliberately limited to the verified recovery.

## Partial and waiting-import queue recovery — September 9

The user's six selected rejection families now qualify completed `importPending`
rows for recovery: existing-file upgrade/revision rejection, no eligible files,
file parsing failure, unexpected quality, and inability to determine whether a
file is a sample. Episode and movie variants of upgrade messages are accepted.
An ordinary pending import or an import already running is still protected.
This implements an explicit replacement policy, not a diagnosis that every
rejected file is corrupt or that a missing path will be fixed by redownloading.

Queue/history preflight resolves the full download scope, including episodes not
shown in the current queue. Grabbed history can resolve missing queue mappings;
cross-subject history and unresolved identities still block. Queue reads include
unknown items so a hidden sibling cannot evade group validation. State and group
membership are rechecked before mutation. A partial pack searches only its missing
episodes. An all-existing group performs cleanup without searches or retry charges.
The guard suppresses searches already issued during that queue scan or covered by
another active download, and rechecks replacements after queue removal.

The initial September 9 implementation used the following retention policy,
superseded by the September 11 client-cleanup correction below. When current
media or prior import history existed, the queue DELETE used
`removeFromClient=false`, `blocklist=true`, and `skipRedownload=true`. Imported
library files are retained, and the guard does not ask the download client to
delete source files that may still be useful or shared. Those files may remain
in the client for operator cleanup. A missing-only download with no prior imports
retains the existing client-removal policy. This queue path never deletes a
managed episode/movie file. Presence of such a file does not validate its subtitles.

That initial operation journal recorded `preserveDownloadFiles`, the full episode scope,
and the actual search scope when requested. Retry reservations cover only missing
search targets, with existing caps and uncertain-operation exclusion retained.
A failed queue removal, later read, or search is not replayed after restart.
The existing recovery opt-in also runs after each successfully listed library in
scan-once mode. Serve keeps startup/hourly checks. No environment defaults changed;
startup logs now include whether queue recovery is enabled.

The [Sonarr queue controller](https://github.com/Sonarr/Sonarr/blob/develop/src/Sonarr.Api.V3/Queue/QueueController.cs)
and [Radarr queue controller](https://github.com/Radarr/Radarr/blob/develop/src/Radarr.Api.V3/Queue/QueueController.cs)
provide independent client-removal and blocklist flags. Their failure services
need grabbed history to publish the failure/blocklist event; without it, a queue
request can otherwise return without blocklisting. Recovery therefore defers
missing grabbed history rather than deleting a download and claiming success.
The [Sonarr failure service](https://github.com/Sonarr/Sonarr/blob/develop/src/NzbDrone.Core/Download/FailedDownloadService.cs)
and [download history service](https://github.com/Sonarr/Sonarr/blob/develop/src/NzbDrone.Core/Download/History/DownloadHistoryService.cs)
document this event and its retained download state.

Local fixtures cover all six message families, partial and all-existing targets,
history-only/unparsed mappings, imported siblings, active and ambiguous shared
downloads, stale snapshots, concurrent duplicate recovery, duplicate releases,
active alternative downloads, replacements arriving during removal, retry caps,
journal failures, missing-history recovery, cancellation, and ambiguous outcomes
across restart. Both scan and serve opt-ins are tested. The final `go test ./...`
pass completed in 16.122 seconds and `go test -race ./...` in 17.527 seconds.
`go vet ./...` and `./lint.ps1` passed; lint reported zero issues. Linux/amd64
application and test compilation also passed. Linux runtime and Docker execution
were not exercised.

The completed September 9 GET-only dry run inspected 192 Sonarr queue rows:
47 eligible rows represented 29 downloads. It planned recovery for 24 downloads,
including six partial groups and eight all-existing groups, with 13 scoped search
plans. Five downloads remained blocked because grabbed history was absent.
Radarr had 11 queue rows; one qualified and planned a movie search. The pass used
227 Sonarr and 14 Radarr GETs in 2.65 seconds. The two duplicate four-episode packs
with two missing episodes produced only one planned search for those missing
targets. A separate check of the exact reported title resolved both downloads,
confirmed episodes 05–06 already imported and 07–08 missing, and verified one
shared replacement-search plan for 07–08 while retaining files. That check used
19 GETs in 0.61 seconds. These are plans, not claims of completed production
remediation.

All requests used forced dry run, independent GET-only transports, and disabled
redirects. There were **zero API mutations, retry-state writes, or media writes**.
The harness uses empty in-memory retry state and cannot establish eligibility
against the running deployment's counters or journal. It does not access download
contents or validate existing subtitles; existing-file protection uses current
Arr metadata. No new full-library probe pass or deployment was performed.

## Download-client cleanup correction — September 11

At the user's request, every queue removal now uses `removeFromClient=true`,
including partial imports, all-existing groups, and downloads with prior import
history. This asks Arr to remove the client task and delete its downloaded files.
The previous client-file retention exception and its optional API flag are gone.
Imported library files remain untouched by queue recovery. Full target validation,
active-import protection, blocklisting, `skipRedownload=true`, missing-only search
scope, retry caps, and uncertain-operation protection are unchanged. There are no
new environment variables or default changes. The operation journal retains full
episode and actual search scopes; new operations no longer record a file-retention
choice. This change does not perform retroactive cleanup of old client tasks.

Every local queue DELETE fixture now requires client removal. Regression tests
cover partial packs, all-existing groups, and prior imports whose media is present
or subsequently missing, for both Arr types. Dry runs still make no mutations or
state writes. Existing restart, cancellation, duplicate-delivery, and concurrency
tests pass. `go test ./...` passed in 16.307 seconds; `go test -race ./...` passed in
17.683 seconds. Vet and lint passed with zero lint issues. Linux/amd64 application
and test compilation passed; Linux runtime and download-client integration were
not exercised.

The fresh live queue dry run completed in 3.76 seconds with forced dry run,
independent GET-only transports, and redirects disabled:

| Instance | Queue rows | Candidate downloads | Recovery plans | Partial groups | All-existing groups | Search plans | GETs |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Sonarr | 153 | 48 | 43 | 7 | 19 | 21 | 398 |
| Radarr | 6 | 2 | 2 | 0 | 0 | 2 | 27 |

All 45 plans explicitly requested client removal. Five Sonarr downloads remained
protected because grabbed history was absent; there were no other preflight
failure categories. The 425 requests made **zero API mutations, retry-state writes,
or media writes**. These checks validate current metadata and planned behavior
using empty retry state, not actual download-client deletion or deployed journal
eligibility. Download contents were not inspected. No deployment was performed.

Follow-up review identified an unresolved shared-download limitation. Both queue
recovery and imported-file origin removal inspect only the current Arr instance.
They do not check whether another Sonarr/Radarr instance needs the same client
task. Queue deletion removes the entire client download, so another consumer
could lose files it still needs. Sonarr's episode queue rows also inherit one
download-level state and message list, rather than independent file readiness.
Current rechecks and same-instance regression tests do not provide cross-instance
coordination or an atomic lock against Arr imports. The shared-group regression
subset passed after this review; no additional behavior change was made.

## Package and repository organization — September 12

The executable now lives in `cmd/arr-guard`. Configuration, Arr API access, media
probing, and path handling have separate `internal/config`, `internal/arr`,
`internal/probe`, and `internal/pathutil` packages. `internal/guard` owns the
application workflows and their durable journal; HTTP serving, scans, and media
remediation are separated into focused files. `internal/testutil` provides shared
test setup. Leaf packages do not import guard. Go module and dependency versions
are unchanged.

The package split retains configuration defaults, JSON state and webhook formats,
mutation ordering, retry scope, platform locks, and uncertain-operation handling.
Probe snapshot fields exposed across the package boundary are explicitly excluded
from JSON. A typed snapshot-change error retains fresh-read retry behavior while
uncertain mutation errors still prevent replay. Arr transport injection preserves
the normal request deadline, redirect refusal, and independent read-only gate.
The previously documented cross-instance shared-download limitation is unchanged.

Full documentation moved to `docs/`; root `AGENTS.md` forwards to the contributor
instructions there. Existing audit logs and coverage profiles moved to ignored
`logs/`, and local binaries moved to ignored `bin/`. The PowerShell launcher builds
the new command and appends console output to a daily file under `logs/`. The
binary still logs JSON to stderr for container collection. `.gitignore` and
`.dockerignore` exclude credentials, logs, local binaries, and verification
artifacts. Docker builds now target `./cmd/arr-guard`; documented run/test commands
and links were updated.

All 127 original test functions remain. Splitting two mixed-purpose tests adds
standalone Unicode-path and unknown-release-year checks; an additional regression
verifies that validation JSON excludes filesystem snapshots. Native tests, race
detection, vet, and lint passed. The initial move damaged two Unicode test strings;
the tests caught it, the original Unicode fixtures were restored, and the complete
suite then passed. A declaration comparison found 196 production declarations
unchanged after package qualification and identifier renaming; the remaining
boundary edits were reviewed separately.

Native command build/help and Linux/amd64 application and test compilation passed.
PowerShell syntax checks and an isolated launcher fixture passed. The launcher
fixture used a temporary fake Go executable and synthetic `.env`, verifying both
output streams, daily append, console output, overrides, and nonzero exit handling
without using the production configuration or network. Linux runtime and Docker
execution remain unverified.

The live compatibility and queue pass completed in 58.372 seconds:

| Instance | Accessible library files | Size mismatches | Compatibility GETs | Queue plans | Queue-plan GETs |
| --- | ---: | ---: | ---: | ---: | ---: |
| Sonarr | 10,923 | 0 | 1,129 | 44 | 397 |
| Radarr | 427 | 0 | 490 | 2 | 27 |

Both real webhook dry runs and stale-snapshot refusals passed. Sonarr's queue plans
included seven partial groups, 19 all-existing groups, and 22 replacement searches;
Radarr planned two movie searches. No queue preflight refusals occurred in this
snapshot. Queue contents had changed independently since the earlier live runs.
The relocated harness resolved the root `.env`, forced dry run, and used independent
GET-only transports with redirects disabled. Its **2,043 GETs made zero API
mutations, retry-state writes, or media changes**. This was a metadata and sample
probe pass, not a new full-library probe pass or production mutation verification.
No deployment was performed. Detailed local outputs remain untracked in `logs/`.

## Additional waiting-import reasons — September 12

The shared queue classifier now accepts the requested movie/series grab-history
warnings ending in "release was matched to movie/series by ID", plus
`Caution: Found executable file`. Matching uses case-insensitive diagnostic prefixes
in queue titles or messages, including messages with trailing details. This applies
to queue recovery in both scan and serve modes. Existing completion, identity,
history, shared-download, and retry checks still apply; defaults are unchanged.

Native tests, race detection, vet, and lint (zero issues) passed. Local HTTP fixtures
verify all three reasons, scoped episode/movie replacements, removal flags, dry-run
state preservation, and refusals for missing/conflicting grabbed history. Production
tests used the independent GET-only transport, disabled redirects, and forced dry
run: 14 Sonarr plans (three all-existing, 11 searches) and one Radarr search plan,
with zero preflight refusals across 141 GETs. There were no API mutations, retry-state
writes, or media changes. These live results cover the current queue; each new
diagnostic and actual mutation sequence was verified with local fixtures.
Logs are saved under ignored `logs/import-reasons-*.log`.

## Subtitle-only diagnostics follow-up — September 12

The MPEG-2-only exception was too narrow for the requested subtitle policy. It
still refused invalid chapter timestamps and JPEG decoder errors even when ffprobe
reported English subtitles successfully. Classification now uses ffprobe's own
codec types and container name. Audio/video decoder error and fatal messages can
continue without requiring valid video dimensions. Container/decoder name collisions
remain ambiguous; a container context permits only the exact chapter end-before-start
timestamp error. Explicit subtitle/caption/user-data errors stay protected even
inside a video decoder. Unknown contexts, unclassified continuation lines, and
other container or stream-discovery errors also stay protected.

This follows FFmpeg's [chapter handling](https://github.com/FFmpeg/FFmpeg/blob/master/libavformat/demux_utils.c)
and [JPEG decoding](https://github.com/FFmpeg/FFmpeg/blob/master/libavcodec/mjpegdec.c):
the former drops an invalid chapter entry, while the latter can report image decode
failure independently of subtitle streams. Full stream discovery stays enabled;
the [ffprobe stream selector](https://ffmpeg.org/ffprobe.html#Main-options) changes
stream output and cannot by itself suppress unrelated initialization errors.

Ignored diagnostics are identified as non-subtitle warnings in logs, with the same
deduplication, address removal, sanitization, and total 2,048-character text budget
as failures. Classification sees all bounded stderr before log truncation. Nonzero
process exit, timeout/cancellation, malformed or oversized output, and changed
media/sidecar snapshots still stop validation. No language or age policy, runtime
configuration defaults, remediation ordering, or retry accounting changed.

Verification:

- Native tests, race detection, vet, and lint (zero issues) passed; Linux/amd64
  application and test compilation passed. The updated local tests cover audio,
  MPEG-2, JPEG attachments, chapter timestamps, English/foreign/missing subtitles,
  caption errors in video decoders, PGS bitmap errors, ambiguous contexts, late
  failures beyond the logging budget, and existing output/snapshot protections.
- Real ffmpeg/ffprobe fixtures reproduce bad chapter timestamps and a malformed
  JPEG attachment both with and without English subtitles. Only disposable test
  files are damaged; probes leave those files unchanged. Local Arr HTTP fixtures
  verify rejection/remediation and successful validation/reset for both servers.
- Both reported production files initially reproduced the refusal. After the
  change they passed repeated probes and scan/webhook dry runs with English
  subtitles detected (16 GETs, zero preflight blocks).
- The historical 19-file live set now contains five file IDs returning HTTP 404;
  that run failed those reads and made 122 GETs. The final 14-current-file run
  completed with 117 GETs: 13 English-subtitle accepts reached both scan and webhook
  preflights, with zero preflight blocks. Radarr file 275 still reports
  `[pgssub] [error] Bitmap dimensions (648x67) invalid.` and stays protected because
  it is a subtitle decoding failure. The unavailable IDs were not treated as passes.

All live checks enforced forced dry run, an independent GET-only transport, and
disabled redirects. They made zero API mutations, retry-state writes, or media
changes. This was targeted verification, not a full-library probe pass; actual
server mutations, Linux runtime, and deployment were not exercised. Logs are in
ignored `logs/subtitle-diagnostics-*.log`.

## Unidentified subtitle inventory follow-up — September 12

At the user's request, an exit-zero probe with a complete JSON stream array and
no identifiable subtitle streams or matching sidecars now fails subtitle policy,
even when media/stream-discovery diagnostics are present. The diagnostic text is
retained in the rejection reason. These failures can complete normal scan/webhook
remediation through the existing origin, snapshot, state, and search checks instead
of remaining probe-error retries. Unknown stream types and subtitle-looking codec
names or English tags do not establish a subtitle or qualify for age grace.

Operational diagnostics such as I/O, permissions, and memory failures still return
errors, including when ffprobe exits zero. Nonzero exit status, incomplete/invalid
JSON, output limits, access failures, and snapshot changes stay protected. When a
subtitle stream or sidecar is identified, the preceding diagnostic rules still
apply; this change does not convert an identified PGS decoding error into a policy
rejection.

Native tests, race detection, vet, lint (zero issues), and Linux/amd64 application
and test compilation passed. Local fixtures cover unidentified/empty inventories,
misleading English metadata, media-discovery errors, operational failures, and
new sidecars appearing before remediation. Both Arr fixtures exercise the full
scan and webhook paths, including deletion, confirmed-history blocklisting, scoped
searches, and dry-run state preservation.

The live three-file check made 18 GETs: both reported chapter/JPEG examples still
pass with English subtitles through repeated probes and scan/webhook dry runs;
the known PGS bitmap failure remains protected. This checks compatibility of the
existing cases. The new unidentified-subtitle branch and mutation sequence were
verified with local fixtures, not by modifying production media. All live checks
forced dry run, used an independent GET-only transport with redirects disabled,
and made zero API mutations, retry-state writes, or media changes. Logs are in
ignored `logs/unidentified-subtitles-*.log`; no deployment was performed.

## Initial audit verification

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

That historical full pass protected 84 candidate remediations: 78 at the automatic
failed-redownload setting check and six at the HTTP deadline. Detailed diagnostics
reproduced all six deadline failures and identified Sonarr's
`GET /api/v3/history/series` as the slow read (13 seconds in an isolated trace).

The initial audit's concurrency fix serialized remediation preflights in both
modes and early webhook origin-history checks, while retaining parallel probes.
The workflow follow-up now performs those origin checks after probing.
Regression tests first reproduced eight overlapping reads, then passed with a
peak of one. All six affected files were submitted concurrently against the live
server after the fix: the batch completed in **2 minutes 5 seconds** with the
normal **30-second per-request deadline**, zero timeouts, and zero mutations.
Each reached the then-required automatic-redownload safety check, since removed
by the follow-up above. Waiting for the
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
continued importing independently. These initial-audit counts are historical;
the workflow follow-up table above records the current verification snapshot.

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
requires authentication. History failure now supports automatic failed
redownload, delegating its replacement search to Arr when applicable. A missing
required setting still stops remediation; the guard never changes server
settings itself. Fully imported queued downloads remain
eligible; incompletely imported packs are retained.

API behavior was checked against the upstream
[Sonarr history controller](https://github.com/Sonarr/Sonarr/blob/main/src/Sonarr.Api.V3/History/HistoryController.cs),
[Radarr history controller](https://github.com/Radarr/Radarr/blob/master/src/Radarr.Api.V3/History/HistoryController.cs),
[Radarr queue controller](https://github.com/Radarr/Radarr/blob/master/src/Radarr.Api.V3/Queue/QueueController.cs),
and [Sonarr completed-download service](https://github.com/Sonarr/Sonarr/blob/main/src/NzbDrone.Core/Download/CompletedDownloadService.cs).
