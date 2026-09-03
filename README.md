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

The handler acknowledges quickly and processes work in a bounded queue. Duplicate webhook deliveries are serialized per media-file ID.

While running with `MODE=serve`, the sidecar also checks each configured Arr
queue at startup and once per hour. It selects only completed items whose
tracked download state is `importBlocked` or whose status text says they are
unable to import automatically. For each matching Radarr item it removes and
blocklists the queue entry, removes it from the download client, and starts a
movie search. For Sonarr, the replacement search is limited to the affected
queue episode ID. The queue check makes only one queue request per Arr instance
per pass and does not inspect media files.

## Remediation behavior

1. Skip movies and episode files released more than 50 years ago before probing or making remediation calls. For Sonarr, the most recent contained episode air date is used, so a multi-episode file remains guarded when it contains a newer episode. This avoids rejecting silent-era media that cannot contain subtitle streams by nature.
2. Probe the managed file. Any ffprobe stream with `codec_type=subtitle` is supported regardless of `codec_name`, including embedded SubRip/SRT, ASS/SSA, WebVTT, MicroDVD/SUB, DVD bitmap/SUP, PGS, and other codecs exposed by ffprobe. Matching sidecars next to the media are also recognized for `.srt`, `.ass`, `.ssa`, `.vtt`, `.sub`, `.idx`, `.sup`, `.pgs`, `.smi`, `.sami`, `.mpl2`, `.ttml`, `.dfxp`, `.usf`, `.scc`, `.stl`, and `.mks` (for example, `Movie.en.srt`). A stream's language/title metadata or a sidecar filename containing `en`, `eng`, `en-US`, or `English` counts as English; an untagged or unrecognized language is recorded as unidentified. Image subtitles are detected from their ffprobe stream and language metadata or sidecar filename; the guard does not OCR subtitle pixels, so an unidentified image stream follows the existing unidentified-language/10-year grace rule rather than being guessed.
3. If subtitles exist but every language is non-English/unidentified, media whose Arr year is more than 10 years old is treated as valid. Media with no subtitles is never accepted by this grace rule.
4. If invalid, delete the managed media file through `/api/v3/episodefile/{id}` or `/api/v3/moviefile/{id}`.
5. If the originating download is known, remove/blocklist it through the queue API, or mark its grabbed history failed. This emits Arr's normal failed-download event.
6. Submit `EpisodeSearch` only for the affected Sonarr episode IDs; the sidecar resolves `episodeFileId` through `/api/v3/episode` during `subtitles` scans when a webhook does not include episodes. If that mapping cannot be established, it refuses deletion/search instead of broadening to a whole-series search. Radarr uses `MoviesSearch`. These explicit searches run even when Arr automatic failed-download redownload is disabled.
7. Persist attempts in `STATE_PATH`. Sonarr retries key by affected episode IDs and Radarr retries by movie. After `MAX_ATTEMPTS`, the invalid file is deleted but another automatic search is not started. This prevents a release/indexer that repeatedly lacks subtitles from looping forever.
8. If deletion succeeds but a later blocklist/failure or replacement-search call fails, the unfinished operation is retained in memory and retried during graceful program shutdown (with a two-minute cleanup window). A forced kill or host failure cannot run this cleanup.

Sonarr's public queue/history failure endpoints identify a download, not an individual episode. The sidecar therefore scopes replacement searches to the affected episode IDs; when one download contains several episodes, Arr may block that shared release for the download as a whole, which is an API limitation.

For an old/manual library file with a matching Arr media-file ID but no matching
import history, the normal rules still apply: an invalid file is deleted and
the affected Sonarr episodes or Radarr movie are searched. Blocklisting is
skipped because Arr has no release identity to mark failed. Files found by the
`unmatched` scan are excluded from these remediation rules entirely.

## Safety

- Set `DRY_RUN=true` to probe and log without deleting or searching.
- Mount media read-only in this sidecar. It only probes the files; Sonarr/Radarr perform deletion in their own container when the sidecar calls the media-file API.
- Protect the webhook endpoint with `WEBHOOK_USERNAME`/`WEBHOOK_PASSWORD` (the native Arr Webhook fields) or `WEBHOOK_TOKEN`; without either, the endpoint accepts requests from any reachable client.
- `ffprobe` failures are non-destructive: the file is left in place and the error is logged.
- After a successful Arr media-file deletion, graceful shutdown retries any unfinished blocklist/failure and replacement-search calls before exiting. Keep the process running long enough for the cleanup window to complete; `SIGKILL` and power loss cannot be recovered in memory.

## Development

```powershell
go test ./...
go vet ./...
./lint.ps1
go run . --help  # set MODE to `serve`, `unmatched`, or `subtitles`
```

`lint.ps1` runs the pinned `golangci-lint` release used by the project.

Repository references checked for this implementation:

- [Sonarr](https://github.com/Sonarr/Sonarr)
- [Radarr](https://github.com/Radarr/Radarr)
