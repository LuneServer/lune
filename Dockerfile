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
RUN if [ -f /etc/apt/sources.list.d/debian.sources ]; then \
      sed -i 's/^Components: main$/Components: main contrib non-free non-free-firmware/' /etc/apt/sources.list.d/debian.sources; \
    fi \
    && apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      ffmpeg \
      gosu \
      libcap2-bin \
      libva-drm2 \
      libvulkan1 \
      mesa-va-drivers \
      mesa-vulkan-drivers \
      tzdata \
      vainfo \
    && if [ "$(dpkg --print-architecture)" = "amd64" ]; then \
      apt-get install -y --no-install-recommends \
        i965-va-driver \
        intel-media-va-driver-non-free \
        libmfx-gen1.2; \
    fi \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 1000 -m -d /data lune

COPY --from=builder /out/lune /usr/local/bin/lune
COPY --from=builder /src/web-dist /web
COPY deploy/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN setcap cap_net_bind_service=+ep /usr/local/bin/lune \
    && chmod +x /usr/local/bin/docker-entrypoint.sh \
    && chmod -R a+rX /web

VOLUME ["/data", "/media"]
EXPOSE 80 443 5863

ENV PUID=1000 \
    PGID=1000 \
    UMASK=002 \
    LUNE_DATA_DIR=/data \
    LUNE_MEDIA_DIR=/media \
    LUNE_WEB_DIR=/web \
    LUNE_LOG_FORMAT=json

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["/usr/local/bin/lune"]
