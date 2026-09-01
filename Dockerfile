FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod .
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/arr-subtitle-guard .

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
	&& useradd --system --uid 10001 --create-home guard \
	&& install -d -o guard -g guard /var/lib/arr-subtitle-guard
COPY --from=build /out/arr-subtitle-guard /usr/local/bin/arr-subtitle-guard
USER guard
WORKDIR /var/lib/arr-subtitle-guard
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/arr-subtitle-guard"]
