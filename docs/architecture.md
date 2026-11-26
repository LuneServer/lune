# Lune — Platform Architecture (v1 Blueprint)

**Organisation:** `github.com/LuneServer`  
**Core Repository:** `LuneServer/lune`  
**Web Repository:** `LuneServer/lune-web`  
**Brand Repository:** `LuneServer/lune-brand`

---

## 1. Core Principles

1. **Standalone Self-Hosted Media Server:** Lune is a ground-up replacement for Jellyfin/Plex — not a wrapper or addon. It owns media scanning, metadata, playback, transcoding, identity, and remote edge delivery directly.
2. **Zero Separate Reverse Proxy:** Lune owns the edge natively — ACME TLS (HTTP-01 / DNS-01), HTTP/2 & HTTP/3, session security, adaptive rate limiting, lockouts, and optional GeoIP/ASN policy are built in.
3. **Web-First, Native-App-Ready:** `lune-web` is the flagship v1 client, backed by a contract-first OpenAPI 3.1 API, RFC 8628 device login, and signed streaming designed for future iOS, tvOS, Android, and desktop apps.
4. **Single-Container Self-Hosting:** Clean Unraid/Docker footprint — one container, one persistent `/data` volume (SQLite WAL + artwork/transcode cache), and read-only `/media` mounts, with optional GPU passthrough (`/dev/dri`) for hardware transcoding.

---

## 2. Technology Stack

### Core Server (`LuneServer/lune`)
- **Language:** Go (`1.26+`)
- **Embedded TLS / ACME:** `certmagic` (HTTP-01, TLS-ALPN-01, and DNS-01 via `libdns`, including Cloudflare DNS)
- **Transport:** stdlib `net/http` (HTTP/1.1, HTTP/2) + `quic-go` (HTTP/3)
- **HTTP Routing & OpenAPI 3.1:** `chi` + `huma` (code-first typed handlers emitting a canonical OpenAPI 3.1 spec)
- **Persistence:** SQLite in WAL mode (`/data/lune.db`) with FTS5 full-text search and versioned SQL migrations
- **Media Probing & Transcoding:** `ffprobe` + `ffmpeg` orchestrated by Lune's transcoder manager with hardware acceleration detection (Intel QSV / VAAPI / NVENC / Apple VideoToolbox)
- **Observability:** Structured JSON logging (`log/slog`), internal/admin-gated `/metrics`, OpenTelemetry hooks

### Web Client & Admin Console (`LuneServer/lune-web`)
- **Framework:** React 19 + TypeScript + Vite (builds to static assets served by `lune` — **no Node.js runtime in production**)
- **Routing & State:** TanStack Router + TanStack Query
- **Styling:** Tailwind CSS v4 mapped directly to `--lune-*` tokens from `LuneServer/lune-brand`
- **API Client:** Auto-generated TypeScript client from `lune`'s OpenAPI 3.1 schema (`openapi-typescript`)
- **Playback:** Native `<video>` + `hls.js`, persistent player state across route navigation

### Future Native Apps (`lune-ios`, `lune-tv`, `lune-android`)
- Generated typed clients from the same OpenAPI 3.1 spec
- Dual auth: Bearer/refresh tokens + **RFC 8628 Device Authorization Flow** (6-char TV pairing code)
- Short-lived HMAC-signed stream URLs compatible with AVPlayer, ExoPlayer, and MPV

---

## 3. System Topology

```text
Internet / LAN Clients (lune-web, future iOS / tvOS / Android apps)
        │  HTTPS (TLS 1.3 · HTTP/2 + HTTP/3)
        ▼
┌─────────────────────────────────────────────────────────────────┐
│  Lune Server (single container · ports 80/443)                  │
│                                                                 │
│  1. Edge Plane (internal/edge)                                  │
│     • Embedded ACME (HTTP-01 / DNS-01 via Cloudflare, etc.)     │
│     • Strict TLS 1.3 defaults, HSTS, security headers           │
│     • Explicit trusted-CIDR proxy mode (disabled by default)    │
│                                                                 │
│  2. Security & Identity Plane (internal/security, internal/auth)│
│     • Adaptive IP + account rate limiting & progressive lockout │
│     • Optional embedded GeoIP/ASN policy                        │
│     • Argon2id passwords, HttpOnly cookies (web),               │
│       JWT/refresh tokens + RFC 8628 device flow (native apps),  │
│       optional OIDC integration                                 │
│                                                                 │
│  3. Web Host (internal/web)                                     │
│     • Serves static lune-web bundle + role-gated /admin console │
│                                                                 │
│  4. Native Media Engine                                         │
│     • Library Scanner (internal/library): fs watcher & indexer  │
│     • Probe & Metadata (internal/metadata): ffprobe, local NFO, │
│       pluggable metadata fetchers (TMDB/TVDB/MusicBrainz)       │
│     • Playback & Sync (internal/playback): watch state, resume  │
│     • Streaming & Transcode (internal/stream):                  │
│         - Zero-copy Direct Play (HTTP Range / sendfile)         │
│         - On-demand HLS Remux / HW Transcode (QSV/VAAPI/NVENC)  │
│         - Short-lived HMAC-signed stream URLs (/stream/{token}) │
└───────────────────────────────┬─────────────────────────────────┘
                                │
             ┌──────────────────┴──────────────────┐
             ▼                                     ▼
   [ /data (RW): SQLite WAL,             [ /media (RO): Movies,
     certs, artwork, transcode cache ]     TV, Music + optional /dev/dri ]
```

---

## 4. Security & Streaming Model

### Direct Play & On-Demand Transcoding
- Clients report codec, container, resolution, HDR, and audio capabilities to `POST /api/v1/items/{id}/playback`.
- Lune selects the cheapest valid path: **Direct Play** (raw byte-range file streaming off disk) → **Direct Stream / Remux** (copy video, remux container or transcode audio only) → **Full Hardware Transcode** (HLS fMP4 segments).
- Lune returns a short-lived HMAC-SHA256 signed URL (`GET /stream/{token}` or `/stream/{token}/master.m3u8`) bound to `user_id`, `item_id`, `session_id`, and expiry — so native players stream cleanly without ever putting long-lived credentials in query parameters.

### Built-In Edge Security
- Because Lune terminates TLS directly, `RemoteAddr` is authentic by default (no spoofable `X-Forwarded-For`). Trusted upstream proxy CIDRs are strictly opt-in.
- Brute-force protection, per-IP/per-account rate limiting, lockouts, and security audit logs live directly inside Lune — no Fail2ban or external WAF scripting required.

---

## 5. `LuneServer/lune` Directory Layout

```text
lune/
├── cmd/
│   └── lune/              # Entrypoint, CLI flags, graceful shutdown
├── internal/
│   ├── config/            # Typed config (YAML + env overrides)
│   ├── edge/              # CertMagic ACME, HTTP/2 + HTTP/3 listener, security headers
│   ├── security/          # Rate limiter, lockout store, GeoIP filter, trusted CIDRs
│   ├── auth/              # Users, sessions, tokens, RFC 8628 device flow, OIDC
│   ├── store/             # SQLite (WAL + FTS5) connection, migrations, repositories
│   ├── library/           # Media library paths, filesystem scanner & watcher
│   ├── metadata/          # ffprobe stream inspection, NFO parser, artwork & remote providers
│   ├── requests/          # Native Media Requests (TMDB Discover, approval workflow, Radarr/Sonarr v3 dispatch)
│   ├── playback/          # Watch progress, resume points, active sessions
│   ├── stream/            # Capability negotiation, HMAC signed URLs, Direct Play & FFmpeg HLS
│   ├── api/               # Huma/Chi HTTP handlers + OpenAPI 3.1 spec export
│   └── web/               # Static bundle handler for lune-web
├── deploy/
│   ├── docker-compose.yml # Single-container Lune deployment (with /media RO & /dev/dri)
│   └── unraid/            # Unraid CA template (later)
├── docs/
│   └── architecture.md    # This document
├── Dockerfile             # Multi-stage static Go binary + FFmpeg runtime image
└── go.mod
```

---

## 6. Milestone Roadmap

- **M0 — Repo & Edge Skeleton:** Go server boots with embedded edge TLS (dev HTTP/self-signed + prod CertMagic ACME), SQLite WAL store + migrations, health & system info endpoints, OpenAPI 3.1 schema export, and single-container `Dockerfile` + `docker-compose.yml`.
- **M1 — Identity & Security Plane:** First-run admin setup, Argon2id auth, cookie sessions (web) + bearer/refresh tokens + RFC 8628 device pairing (apps), adaptive rate limiting & audit log.
- **M2 — Library Scanner & Metadata Engine:** Library configuration, filesystem watcher, `ffprobe` media stream extraction, local artwork/NFO support, and TMDB metadata matching.
- **M3 — Streaming & Playback Engine:** Capability negotiation, HMAC-signed Direct Play (`Range`), on-demand FFmpeg remux / hardware-accelerated HLS transcoding, and watch-progress sync.
- **M4 — `lune-web` MVP:** Dark-first celestial web UI built on `lune-brand` tokens + generated OpenAPI client, library browsing, player, and admin console (TLS certs, libraries, hardware accel, active streams, security log).
- **M5 — Distribution & Native Apps:** Multi-arch `ghcr.io/luneserver/lune` images, Unraid template, and first native TV/mobile clients against the stable v1 OpenAPI contract.
