# syntax=docker/dockerfile:1

ARG GO_VERSION=1.26

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG GIT_COMMIT=unknown
ARG GIT_VERSION=unknown

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/arr-guard .

FROM debian:bookworm-slim

ARG GIT_COMMIT=unknown
ARG GIT_VERSION=unknown

LABEL org.opencontainers.image.title="arr-guard" \
    org.opencontainers.image.description="Subtitle validation sidecar for Sonarr and Radarr" \
    org.opencontainers.image.revision=$GIT_COMMIT \
    org.opencontainers.image.version=$GIT_VERSION

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --create-home guard \
    && install -d -o guard -g guard /var/lib/arr-guard

COPY --from=build /out/arr-guard /usr/local/bin/arr-guard
COPY --from=mwader/static-ffmpeg:7.1.1 /ffprobe /usr/local/bin/ffprobe
ENV FFPROBE_PATH=/usr/local/bin/ffprobe
USER guard
WORKDIR /var/lib/arr-guard
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/arr-guard"]
