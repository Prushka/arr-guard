#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="${IMAGE:-meinya/arr-guard}"
PLATFORM="linux/amd64"
GIT_COMMIT="$(git -C "$ROOT_DIR" rev-parse --short=7 HEAD)"

if [[ -z "$GIT_COMMIT" ]]; then
    echo "Unable to determine the current git commit" >&2
    exit 1
fi

if [[ -n "$(git -C "$ROOT_DIR" status --porcelain)" ]]; then
    GIT_COMMIT="${GIT_COMMIT}-dirty"
fi

output=(--push)
case "${PUSH:-true}" in
    1|true|TRUE|yes|YES)
        output=(--push)
        ;;
    0|false|FALSE|no|NO)
        output=(--load)
        ;;
    *)
        echo "PUSH must be true or false" >&2
        exit 1
        ;;
esac

echo "Building ${IMAGE}:${GIT_COMMIT} for ${PLATFORM}"
docker buildx build \
    --platform "$PLATFORM" \
    --build-arg "GIT_COMMIT=$GIT_COMMIT" \
    --build-arg "GIT_VERSION=$GIT_COMMIT" \
    --tag "${IMAGE}:${GIT_COMMIT}" \
    --tag "${IMAGE}:latest" \
    "${output[@]}" \
    --file "$ROOT_DIR/Dockerfile" \
    "$ROOT_DIR"
