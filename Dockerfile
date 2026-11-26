# syntax=docker/dockerfile:1

FROM golang:1.26-bookworm AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=0.1.0-dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
RUN --mount=type=secret,id=tmdb_api_key,required=false \
    TMDB_KEY="$(cat /run/secrets/tmdb_api_key 2>/dev/null | tr -d '\r\n' || true)" && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags="-s -w -X github.com/LuneServer/lune/internal/version.Version=${VERSION} -X github.com/LuneServer/lune/internal/version.Commit=${COMMIT} -X github.com/LuneServer/lune/internal/version.BuildDate=${BUILD_DATE} -X github.com/LuneServer/lune/internal/config.DefaultTMDBAPIKey=${TMDB_KEY}" \
      -o /out/lune ./cmd/lune

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      ffmpeg \
      libcap2-bin \
      tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 1000 -m -d /data lune

COPY --from=builder /out/lune /usr/local/bin/lune
COPY --from=builder /src/web-dist /web
RUN setcap cap_net_bind_service=+ep /usr/local/bin/lune \
    && chown -R lune:lune /web

VOLUME ["/data", "/media"]
EXPOSE 80 443 5863

ENV LUNE_DATA_DIR=/data \
    LUNE_MEDIA_DIR=/media \
    LUNE_WEB_DIR=/web \
    LUNE_LOG_FORMAT=json

USER lune
ENTRYPOINT ["/usr/local/bin/lune"]
