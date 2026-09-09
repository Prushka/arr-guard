# Arr Guard

A Go sidecar for Sonarr and Radarr that rejects library files without subtitles or without English subtitles. It uses `ffprobe` for embedded streams, recognizes matching external subtitle files, and uses Arr's REST APIs for deletion, blocklisting, and replacement searches.

## Why a sidecar

Sonarr and Radarr do not expose a general-purpose server add-on/plugin directory. Their repositories do provide two supported external integration points:

- `Connections -> Webhook`: post-import `Download` events contain the imported file ID/path and download ID.
- `Media Management -> Import Extra Files -> Script Import`: a script can run before a file is moved, but a failed script is treated as an import problem and does not itself guarantee blocklisting or a replacement search.

This sidecar uses the webhook for new imports and the REST API for the complete remediation workflow. `MODE=subtitles` handles the existing library once.

## Run

Set at least one Arr instance. API keys are read only from environment variables.

```powershell
$env:SONARR_URL = "http://localhost:8989"
$env:SONARR_API_KEY = "..."
$env:RADARR_URL = "http://localhost:7878"
$env:RADARR_API_KEY = "..."
$env:PATH_MAPPINGS_JSON = '[{"from":"/tv","to":"D:\\Media\\TV"},{"from":"/movies","to":"D:\\Media\\Movies"}]'
$env:MODE = "serve"
go run .
```

Run the one-time scan before enabling webhooks:

```powershell
$env:MODE = "subtitles"
go run .
```

To inspect media files beneath the mapped library roots that are not present
in either Arr library, run the read-only orphan scan:

```powershell
$env:MODE = "unmatched"
go run .
```

It scans only video files, writes every file with no matching Arr media-file ID
to `unmatched.json` (or the path in `UNMATCHED_PATH`), and performs no ffprobe,
deletion, blocklisting, searching, or retry-state updates. Set
`UNMATCHED_EXCLUDE_DIRS` to a comma-separated list of directories to skip during
this scan; relative entries are resolved beneath each mapped scan root. Orphan
files are not part of `subtitles` or webhook handling, because they have no Arr
media-file ID to remediate. The report contains paths only (plus scan metadata),
with no subtitle validation field.

For Docker, pull and start the Docker Hub image:

```bash
docker compose pull
docker compose up -d
```

The Compose files always use `meinya/arr-guard:latest` from Docker Hub; the
image name and tag are not configurable through Compose environment variables.
The image includes the static `ffprobe` binary from
`mwader/static-ffmpeg:7.1.1` and Compose selects it at
`/usr/local/bin/ffprobe`.
To publish a new image from this repository, `build.sh` uses Docker Buildx for
`linux/amd64`, tags it with the short commit hash and `latest`, and pushes both
tags by default. Set `PUSH=false` to load the image into the local Docker
engine instead. Set `IMAGE` only when publishing a different registry image:

```bash
IMAGE=registry.example.com/media/arr-guard PUSH=true ./build.sh
```

The Compose deployment reads Arr credentials from `.env`, mounts
`MEDIA_ROOT` (default `/srv/media`) at `/media`, and uses an identity
`PATH_MAPPINGS_JSON` because Arr and the sidecar share that container path.
Set `MEDIA_ROOT` and adjust the mapping if the Arr containers use a different
path. Never mount only a download directory: the sidecar needs the managed
library files too. The example remains available in
[`docker-compose.example.yml`](docker-compose.example.yml).

## Arr webhook setup

Create one Webhook connection in each Arr instance:

- Sonarr URL: `http://arr-guard:8080/webhook/sonarr`
- Radarr URL: `http://arr-guard:8080/webhook/radarr`
- Method: `POST`
- Authentication: set Sonarr/Radarr's Webhook `Username` and `Password` fields to `WEBHOOK_USERNAME` and `WEBHOOK_PASSWORD`. The sidecar also accepts `X-Webhook-Token: <WEBHOOK_TOKEN>` or `Authorization: Bearer <WEBHOOK_TOKEN>` when `WEBHOOK_TOKEN` is configured.
- Enable the `On Download` event. In Sonarr, `On Import Complete` may also be enabled to cover batch imports; both payload shapes are supported. `Grab` is not needed.

In write-enabled mode the handler persists a minimal job before acknowledging
HTTP 202. Temporary read/probe failures and incomplete imports remain queued,
retrying after 1, 2, 4, 8, 16, and 32 minutes, then hourly until resolved.
These retries fetch current metadata and probe again; failed checks do not grab
media or consume remediation attempts. Identity conflicts, missing required
configuration, and uncertain mutations pause for review. Duplicate deliveries are
serialized by media-file ID. Dry-run serving uses an in-memory queue and never
writes retry state or schedules these durable retries.

A one-time `MODE=subtitles` scan makes up to three attempts for temporary failures,
waiting one and then two seconds and refreshing file metadata before reprobing.
It continues other files and configured servers. In write mode, unresolved safe
per-file work is saved for a later `MODE=serve` process using the same `STATE_PATH`.
The scan still exits with an error if work remains unresolved; it does not wait
indefinitely. Dry run saves no jobs. If the library cannot be listed at all, or
Arr is unavailable during startup, a later restart or scan is needed. Serve mode
handles import webhooks and saved work; it does not periodically rescan the library.

Queue recovery is disabled by default. Set `RECOVER_BLOCKED_QUEUE=true` to inspect
blocked completed downloads at startup and hourly in `MODE=serve`. This is a
separate policy: an import can be blocked by a permissions problem or a valid
release needing manual selection, so review dry-run output before opting in.

Recovery uses paginated queue reads, groups all entries sharing a download ID,
and rechecks their state. Every entry must still be blocked, belong to the same
subject, and have no managed replacement. One queue removal/blocklist is followed
by a movie search or an episode search covering the affected queue episodes that
are still missing. Replacements arriving after removal are omitted from the search.
`MAX_ATTEMPTS` applies to this path too. A failed removal is never inferred to
have succeeded merely because its queue entry disappeared.

## Remediation behavior

1. Read authoritative file ownership, paths, and release dates from Arr. Webhook
   paths, download IDs, episode IDs, and dates are not trusted for remediation.
   Skip media older than 50 years. Sonarr uses the newest episode in a file;
   an unknown episode date prevents the age exclusion. This is an age policy,
   not an assertion that all older media is silent or cannot have subtitles.
2. Probe the nonempty local file with ffprobe and inspect matching subtitle
   sidecars. All embedded codecs reported as `codec_type=subtitle` are supported,
   including bitmap subtitles. Sidecars support `.srt`, `.ass`, `.ssa`, `.vtt`,
   `.sub`, `.idx`, `.sup`, `.pgs`, `.smi`, `.sami`, `.mpl2`, `.ttml`, `.dfxp`,
   `.usf`, `.scc`, `.stl`, and `.mks`. Empty/nonregular matching sidecars stop
   validation, allowing incomplete imports to finish. Image pixels are not OCR'd;
   stream language/title metadata and sidecar filenames determine language.
   Error-level ffprobe diagnostics stop validation even when the process exits
   successfully; malformed output, cancellation, and timeouts also stop it.
3. Accept English subtitles (`en`, `eng`, `en-US`, `English`). For media more than
   ten years old, at least one unidentified subtitle language also qualifies.
   Known non-English subtitles alone do not qualify for that grace rule, and
   media with no subtitles is never accepted by it.
4. Before deletion, resolve import history and the full current Sonarr episode
   mapping. Failed history reads, ambiguous identity, changed media, incomplete
   imports, and unassigned files stop remediation. Temporary unassigned imports
   wait for their episode mapping and retry. Valid files need no origin
   history. If a webhook supplies a download ID, a rejected file waits for matching
   import history before remediation; a conflicting origin requires review.
   A queued download is left untouched until Arr reports it fully imported;
   queued Sonarr episodes must all have managed files. Old/manual files with no
   import history can still be rejected and searched, without blocklisting.
5. Queue failure uses `skipRedownload=true`, followed by the guard's own scoped
   search. History failure uses a confirmed grabbed record and respects Arr's
   automatic redownload settings. When `autoRedownloadFailed` is enabled, Arr
   handles the replacement search and the guard skips its duplicate search.
   Releases grabbed through interactive search also require
   `autoRedownloadFailedFromInteractiveSearch` to be enabled for Arr to search;
   otherwise the guard searches. The guard reads these settings before deletion
   and again immediately before marking history failed; it never changes them.
   A failed read or missing setting needed for that decision stops remediation.
   See the [Sonarr automatic redownload handler](https://github.com/Sonarr/Sonarr/blob/main/src/NzbDrone.Core/Download/RedownloadFailedDownloadService.cs)
   and [Radarr automatic redownload handler](https://github.com/Radarr/Radarr/blob/master/src/NzbDrone.Core/Download/RedownloadFailedDownloadService.cs).
6. Serialize remediation preflights in both dry run and write mode, keeping probes
   parallel. This avoids overlapping expensive Arr history queries, including
   webhook origin checks after probing. Recheck the Arr record, local media,
   matching subtitle files, directory identity, and episode mapping. Unrelated
   posters, NFO files, and other episode imports do not invalidate the snapshot.
   Persist an operation phase in `STATE_PATH` **before** each mutation.
   Delete only the managed file through Arr,
   then fail/blocklist its confirmed origin. If Arr handles the replacement,
   complete the operation without issuing a guard search; otherwise search the
   still-missing movie or affected episodes within the guard's retry budget.
   If some episodes already have replacements, search only the missing subset;
   if all targets have files, complete without a search. Replacement presence
   avoids a duplicate search but does not establish that its subtitles are valid;
   its import webhook or a later scan performs that check.
   Never issue a series-wide search. An origin already marked failed does not
   trigger a new failure event, so the guard still searches for its missing file.
7. Persist retry counters per movie or individual episode, so file renames and
   single/combined episode releases cannot reset the limit. Valid replacements
   clear those counters in both library scans and webhook jobs only if the media
   and matching-subtitle snapshots still match. Legacy composite episode keys
   migrate to each episode's maximum count. The guard issues
   replacement searches only within `MAX_ATTEMPTS` remediation attempts; a later
   invalid managed file is still deleted and blocklisted without another guard
   search. Arr's automatic redownload can still search at that point: its searches
   are controlled by Arr's settings and cannot be capped by the guard through the
   history-failure API. Queue recovery stops before removing another download
   once its limit is reached.

Sonarr's failure endpoints can blocklist an entire shared release. The sidecar
scopes its own searches to affected episodes, but Arr's automatic recovery can
search the shared release's episodes or an entire season. The sidecar cannot make
Arr's public failure API operate on only part of a download.

## Safety and recovery

- `DRY_RUN` defaults to `true`. Set it explicitly to `false` only when you want
  remediation. Malformed booleans and integers fail startup. Write-enabled
  `MODE=serve` requires webhook authentication. Unmatched mode always uses a
  read-only Arr client, even if `DRY_RUN=false`.
- Dry run performs read/probe/preflight checks without API mutations or retry-state
  writes. The Arr client independently rejects non-GET requests in read-only mode
  and does not follow redirects. `unmatched` writes only its local JSON report.
- Mount media read-only. The guard never edits local media; Arr deletion APIs can
  still delete files through Arr's own mounts when write mode is enabled.
- State/report paths must be distinct `.json` paths outside mapped media roots.
  Keep state on a local durable filesystem. An exclusive OS lock protects the
  state writer, and state is bound to the configured server URLs. Run only one
  write-enabled deployment per Arr server, sharing its single state path; separate
  state paths do not coordinate with each other.
- API timeouts/errors can follow a committed mutation. An unfinished operation is
  retained in the durable `operations` journal and blocks further remediation for
  that subject/download. Shutdown and restart do **not** replay its mutations.
  Persistence failure disables further state writes for that process.
- Before history failure, the journal's `automaticSearch` flag records whether
  Arr is expected to handle replacement. A failed or uncertain history request
  never triggers a fallback guard search. Settings can still change externally
  between the last read and Arr's handling of the failure; those requests are not
  an atomic transaction. Automatic handling means a search is delegated, not that
  a suitable replacement was found or downloaded.
- To reconcile: stop the guard, back up its state JSON, inspect the journal's phase
  and Arr's file/history/queue/command state, and finish or abandon the operation
  deliberately. Remove only the reviewed operation entry, retain its attempt
  counters, and mark its key `true` in `completed` to prevent duplicate work.
  Do not erase the entire state to clear one problem.
- Temporary failures no longer exhaust webhook jobs. Existing jobs stopped at the
  old five-failure limit resume fresh checks after upgrading, respecting their
  saved backoff and operation journal. New permanent or uncertain failures set
  `needsReview: true`. After resolving their cause, redeliver the webhook to
  recheck it; redelivery cannot bypass an unfinished operation. Alternatively,
  with the guard stopped and state backed up, set only that job's `needsReview`
  to `false`, `failures` to `0`, and remove `nextAttempt`, then restart.
- An unfinished Sonarr operation still excludes further remediation throughout
  that series, including other episodes. A history failure or Arr automatic
  search can affect a shared release or season; narrowing this exclusion solely
  to file episode IDs could overlap an uncertain operation. Resolve the journal
  and reactivate any affected review jobs before further remediation. Valid files
  and other series can still be processed.
- Failed probes, empty/nonregular matching sidecars, active shared downloads, and
  unavailable history never authorize deletion. Safe pre-mutation failures get
  later fresh checks; persistent problems still need attention. Queue recovery
  remains opt-in, guard searches retain their attempt cap, and uncertain mutation
  outcomes require reconciliation. State persistence failure disables state writes
  until the underlying problem is resolved and the guard is restarted.

The Arr APIs provide no cross-request transaction or conditional file deletion.
The rechecks narrow races with other Arr activity, but cannot eliminate changes
made by Arr or another application in the interval between a check and mutation.
Likewise, a power/storage failure is subject to the filesystem's durability
support. Review the journal after any uncertain outcome.

## Development

```powershell
go test ./...
go test -race ./...
go vet ./...
./lint.ps1
go run . --help  # set MODE to `serve`, `unmatched`, or `subtitles`
```

`lint.ps1` runs the pinned `golangci-lint` release used by the project.

Opt-in live verification loads `.env`, forces dry run, and adds a separate
GET-only transport with an endpoint allowlist. It never invokes server mutations.
The full scan reads media and writes its orphan report to a local temporary
directory; logs contain counts, numeric file IDs, and error categories rather
than private paths:

```powershell
$env:ARR_LIVE_READ_ONLY = "1"
go test -run '^TestLiveReadOnly$' -v -timeout 3h
$env:ARR_LIVE_FULL_SCAN = "1"
$env:ARR_LIVE_WEBHOOK_CHECK = "1"
go test -run '^TestLiveReadOnly$' -v -timeout 3h
```

Mutation behavior is verified with local `httptest` servers and fault injection.
Live-test success means the harness completed; inspect its probe-error and
preflight-block counts to see which media or actions were refused safely.
`ARR_LIVE_WORKERS` can set 1–8 simultaneous probes (default 4).
`ARR_LIVE_ORIGIN_DIAGNOSTICS=1` additionally inspects imported queue groups and
Sonarr import-history ambiguities, then probes affected candidates to explain
safety refusals. It uses the same GET-only transport and dry-run checks.
To retest particular Sonarr files concurrently, set `ARR_LIVE_PREFLIGHT_IDS` to
their comma-separated numeric IDs (at most 20) and run
`go test -run '^TestLiveTargetedPreflight$' -v -timeout 20m` with the read-only
opt-in enabled. This uses the application's normal request deadline and logs
slow API endpoint names without private URLs or query values.
Real ffprobe tests generate disposable media fixtures when ffmpeg is available.
To verify replacement-search ownership for previously rejected files on either
server, set `ARR_LIVE_REMEDIATION_IDS=sonarr:123,radarr:456` using actual scan IDs
and run `go test -run '^TestLiveRemediationSearchOwnership$' -v -timeout 15m`
with `ARR_LIVE_READ_ONLY=1`. At most 20 files are accepted. This checks real
probes, history preflights, and webhook processing using only GET/dry-run calls.
See [AUDIT.md](AUDIT.md) for the audit's results and limitations.

Repository references checked for this implementation:

- [Sonarr](https://github.com/Sonarr/Sonarr)
- [Radarr](https://github.com/Radarr/Radarr)
