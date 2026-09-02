# Arr Subtitle Guard

A Go sidecar for Sonarr and Radarr that rejects library files without subtitles or without English subtitles. It uses `ffprobe` for embedded streams, recognizes matching external subtitle files, and uses Arr's REST APIs for deletion, blocklisting, and replacement searches.

## Why a sidecar

Sonarr and Radarr do not expose a general-purpose server add-on/plugin directory. Their repositories do provide two supported external integration points:

- `Connections -> Webhook`: post-import `Download` events contain the imported file ID/path and download ID.
- `Media Management -> Import Extra Files -> Script Import`: a script can run before a file is moved, but a failed script is treated as an import problem and does not itself guarantee blocklisting or a replacement search.

This sidecar uses the webhook for new imports and the REST API for the complete remediation workflow. `audit` handles the existing library once.

## Run

Set at least one Arr instance. API keys are read only from environment variables.

```powershell
$env:SONARR_URL = "http://localhost:8989"
$env:SONARR_API_KEY = "..."
$env:RADARR_URL = "http://localhost:7878"
$env:RADARR_API_KEY = "..."
$env:PATH_MAPPINGS_JSON = '[{"from":"/tv","to":"D:\\Media\\TV"},{"from":"/movies","to":"D:\\Media\\Movies"}]'
go run . serve
```

Run the one-time scan before enabling webhooks:

```powershell
go run . audit
```

For Docker, start from [`docker-compose.example.yml`](docker-compose.example.yml). The media paths in `PATH_MAPPINGS_JSON` must resolve to the same files mounted in the sidecar. Never mount only a download directory: the sidecar needs the managed library files too.

## Arr webhook setup

Create one Webhook connection in each Arr instance:

- Sonarr URL: `http://arr-subtitle-guard:8080/webhook/sonarr`
- Radarr URL: `http://arr-subtitle-guard:8080/webhook/radarr`
- Method: `POST`
- Authentication: set Sonarr/Radarr's Webhook `Username` and `Password` fields to `WEBHOOK_USERNAME` and `WEBHOOK_PASSWORD`. The sidecar also accepts `X-Webhook-Token: <WEBHOOK_TOKEN>` or `Authorization: Bearer <WEBHOOK_TOKEN>` when `WEBHOOK_TOKEN` is configured.
- Enable the `On Download` event. In Sonarr, `On Import Complete` may also be enabled to cover batch imports; both payload shapes are supported. `Grab` is not needed.

The handler acknowledges quickly and processes work in a bounded queue. Duplicate webhook deliveries are serialized per media-file ID.

## Remediation behavior

1. Probe the managed file. Any ffprobe stream with `codec_type=subtitle` is supported, including embedded SubRip/SRT, ASS/SSA, WebVTT, MicroDVD/SUB, DVD bitmap/SUP, PGS, and other codecs exposed by ffprobe. Matching sidecars next to the media are also recognized for `.srt`, `.ass`, `.ssa`, `.vtt`, `.sub`, `.idx`, `.sup`, `.smi`, `.sami`, `.mpl2`, `.ttml`, `.dfxp`, `.usf`, `.scc`, `.stl`, and `.mks` (for example, `Movie.en.srt`). A stream or sidecar tagged `en`, `eng`, `en-US`, or `English` counts as English; an untagged or unrecognized language is recorded as unidentified.
2. If subtitles exist but every language is non-English/unidentified, media whose Arr year is more than 10 years old is treated as valid. Media with no subtitles is never accepted by this grace rule.
3. If invalid, delete the managed media file through `/api/v3/episodefile/{id}` or `/api/v3/moviefile/{id}`.
4. If the originating download is known, remove/blocklist it through the queue API, or mark its grabbed history failed. This emits Arr's normal failed-download event.
5. Submit `EpisodeSearch` only for the affected Sonarr episode IDs; the sidecar resolves `episodeFileId` through `/api/v3/episode` during audit when a webhook does not include episodes. If that mapping cannot be established, it refuses deletion/search instead of broadening to a whole-series search. Radarr uses `MoviesSearch`. These explicit searches run even when Arr automatic failed-download redownload is disabled.
6. Persist attempts in `STATE_PATH`. Sonarr retries key by affected episode IDs and Radarr retries by movie. After `MAX_ATTEMPTS`, the invalid file is deleted but another automatic search is not started. This prevents a release/indexer that repeatedly lacks subtitles from looping forever.

Sonarr's public queue/history failure endpoints identify a download, not an individual episode. The sidecar therefore scopes replacement searches to the affected episode IDs; when one download contains several episodes, Arr may block that shared release for the download as a whole, which is an API limitation.

For an old/manual library file with no matching import history, the file is deleted and the affected Sonarr episodes or Radarr movie are searched, but no historical release can be blocklisted because Arr has no release identity to mark failed.

## Safety

- Set `DRY_RUN=true` to probe and log without deleting or searching.
- Mount media read-only in this sidecar. It only probes the files; Sonarr/Radarr perform deletion in their own container when the sidecar calls the media-file API.
- Protect the webhook endpoint with `WEBHOOK_USERNAME`/`WEBHOOK_PASSWORD` (the native Arr Webhook fields) or `WEBHOOK_TOKEN`; without either, the endpoint accepts requests from any reachable client.
- `ffprobe` failures are non-destructive: the file is left in place and the error is logged.

## Development

```powershell
go test ./...
go vet ./...
go run . --help  # modes are `serve` (default) and `audit`
```

Repository references checked for this implementation:

- [Sonarr](https://github.com/Sonarr/Sonarr)
- [Radarr](https://github.com/Radarr/Radarr)
