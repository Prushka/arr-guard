# GitHub image builds

[`docker.yml`](../.github/workflows/docker.yml) tests and builds the existing
`meinya/arr-guard` image for `linux/amd64` on every branch push, pull request, and
manual workflow run. A local commit starts nothing until it is pushed to GitHub.
A push containing several commits builds the branch tip for that push.

## What runs

1. Install Go from `go.mod` and ffmpeg for disposable media fixtures, then run
   formatting checks, the full test suite with race detection, vet, the repository's
   pinned lint script, and actionlint with ShellCheck.
2. Build and load the Docker image. Check the application help command, bundled
   ffprobe, and non-root UID with network disabled, a read-only container filesystem,
   and no mounted media or configuration.
3. On branch pushes/manual runs, publish that tested image as
   `meinya/arr-guard:<7-character-commit>` when both Docker Hub secrets exist.
   Pull requests never log in or publish. Missing secrets skip publishing while
   keeping tests/builds enabled; the run summary explains the skip. Invalid or
   unauthorized credentials fail publishing instead of reporting success.
4. For the default branch (currently `master`), promote the published digest to
   `meinya/arr-guard:latest` only if the commit is still the current branch head.
   Latest-tag promotions are serialized and older builds are skipped. Commit-tag
   builds are independent, so that serialization does not cancel their publishing.

Actions are pinned to release commit SHAs, the GitHub token has read-only repository
permissions, and checkout does not persist credentials. Docker credentials are used
only by publishing steps. CI never receives `.env`, Arr credentials, media mounts,
or a live-test opt-in. Publishing an image does not restart or update a running
Arr Guard installation.

## Add the Docker Hub secrets

1. Ensure the Docker Hub repository `meinya/arr-guard` exists and your Docker Hub
   account can push to it.
2. In Docker account settings, open **Personal access tokens**, select **Generate
   new token**, and create a token with **Read & Write** access. Copy the generated
   token. See [Docker's token instructions](https://docs.docker.com/security/access-tokens/personal-access-tokens/).
3. Open the GitHub repository's **Settings → Secrets and variables → Actions →
   New repository secret**. Add these two repository secrets:

   | Secret | Value |
   | --- | --- |
   | `DOCKERHUB_USERNAME` | Your Docker Hub login username with access to `meinya/arr-guard` |
   | `DOCKERHUB_TOKEN` | The Docker Hub access token from step 2 |

   The repository settings are available at
   [Prushka/arr-guard Actions secrets](https://github.com/Prushka/arr-guard/settings/secrets/actions).
   See [GitHub's secret instructions](https://docs.github.com/en/actions/how-tos/write-workflows/choose-what-workflows-do/use-secrets).
4. Commit and push the workflow if it is not on GitHub yet. Once it is on the
   default branch, select **Actions → Test and publish Docker image → Run workflow**,
   or push another commit. A run started after adding the secrets can publish.

No repository `.env` values, Docker Hub password, extra GitHub token, or application
environment variables are required. The image name is fixed in the workflow to
match the Compose files and `build.sh`; changing registries requires updating those
references together.

## Validation and limits

On September 13, 2026, local verification passed Go formatting, `go test -race
-count=1 ./...`, `go vet ./...`, `./lint.ps1` (zero issues), actionlint v1.7.12 with
ShellCheck v0.11.0, and syntax checks for every inline Bash script. Isolated script
tests used fake credentials and mocked Docker/GitHub commands to verify missing
and partial credentials, successful commit-tag publishing, smoke-test/push failures,
invalid digests, stale branch heads, and failed branch-head reads. Logs are kept
under ignored `logs/`.

Docker is not installed on the local validation host. Container build/execution,
the Linux CI runner, GitHub workflow execution, and a real Docker Hub push therefore
remain unverified here. The first GitHub run performs the container build and smoke
tests even without publishing credentials. No image was pushed or service deployed
during local testing.
