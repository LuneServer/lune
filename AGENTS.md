# AGENTS.md — `LuneServer/lune` (Core Media Server)

This document describes the architecture, conventions, and engineering invariants of **Lune Server** (`github.com/LuneServer/lune`) and how it fits into the wider Lune platform.

> **Mandatory Branch Workflow:** Never commit directly to `main` or pile unrelated changes onto an existing branch. Start every distinct task (feature, fix, or doc change) on its own branch from an up-to-date local `main` (fetch and fast-forward `main` first, then `git switch -c <type>/<short-topic> main`, e.g. `fix/unraid-permissions`; if local `main` cannot fast-forward to `origin/main` or has commits not on `origin/main`, use the freshly fetched `origin/main` as the start point instead). For parallel or concurrent work, or when the working tree already holds uncommitted changes from another task, use a separate worktree (`git worktree add ../lune-worktrees/<topic>/lune -b <type>/<topic> main`, substituting the freshly fetched `origin/main` as the base under the same fallback) instead of switching branches in place; for cross-repo tasks put all three worktrees under the same `../lune-worktrees/<topic>/` parent (`lune`, `lune-web`, `lune-brand`) so the `../lune-web`/`../lune-brand` paths below resolve to the task branches. Keep each branch scoped to one concern, run `git status` before editing to confirm what you're building on, and when a task spans repos (`lune`, `lune-web`, `lune-brand`), use a same-named branch in each. Do not merge, rebase onto, or push unless the user asks.

> **Mandatory Maintenance Rule:** Whenever you implement new functionality or change existing architecture (packages, HTTP endpoints, SQLite migrations, environment variables, streaming behavior, deployment manifests, or cross-repo contracts), **you must update this `AGENTS.md` in the same change**. If the change alters platform-level architecture, also update [`docs/architecture.md`](docs/architecture.md), and if it changes API contracts or static asset expectations consumed by the web client, update `lune-web/AGENTS.md` accordingly.

---

## 1. Lune Platform Ecosystem

Lune is a standalone, single-container self-hosted media platform (*"Your universe. Your media."*) built across three repositories under `github.com/LuneServer`:

| Repo | Local Path | Role |
|---|---|---|
| **`LuneServer/lune`** (this repo) | `../lune` | Core Go server: embedded ACME/HTTPS edge, security/auth, SQLite catalog, library scanner/watcher, metadata enrichment, Direct Play & FFmpeg HLS streaming, OpenAPI 3.1 API, and static SPA host. |
| **`LuneServer/lune-web`** | `../lune-web` | Flagship React 19 + TypeScript + Vite + Tailwind CSS v4 web client and admin console. Compiles to a static `dist/` bundle served directly by `lune` via `LUNE_WEB_DIR` (no Node.js runtime in production). |
| **`LuneServer/lune-brand`** | `../lune-brand` | Visual identity source of truth: Horizon SVG/PNG marks, `BRAND.md`, and dark-first celestial design tokens (`tokens.css`, `tokens.json`). |

Future native clients (`lune-ios`, `lune-tv`, `lune-android`) will consume the same OpenAPI 3.1 contract (`/openapi.json`), Bearer/refresh auth + RFC 8628 device login, and HMAC-signed streaming endpoints.

---

## 2. Core Architectural Principles

1. **Zero Separate Reverse Proxy:** Lune owns TLS termination natively via embedded CertMagic ACME (HTTP-01 / TLS-ALPN-01, plus Cloudflare DNS-01). `RemoteAddr` is authentic by default; `X-Forwarded-For` is ignored unless the peer matches explicitly configured `LUNE_TRUSTED_PROXIES`.
2. **Single-Container, Zero-CGO Footprint:** Static Go binary (`CGO_ENABLED=0`) using pure-Go SQLite (`modernc.org/sqlite`), packaged with `ffmpeg`/`ffprobe` in Debian slim (`Dockerfile`). Persistent state lives in `/data`; media mounts are read-only (`/media`).
3. **Contract-First HTTP API:** JSON API endpoints are registered through `huma/v2` on `chi`, emitting a canonical OpenAPI 3.1 spec at `GET /openapi.json` and interactive docs at `GET /docs`.
4. **Signed Media Delivery:** Clients negotiate playback via `POST /api/v1/items/{id}/playback` and receive short-lived HMAC-SHA256 signed URLs (`/stream/direct/{token}`, `/stream/hls/{token}/{file}`, `/stream/sub/{token}/{file}`) so players never send cookies or long-lived Bearer tokens in media URLs.

---

## 3. Repository & Package Layout

```text
lune/
├── cmd/lune/main.go             # CLI entrypoint (--version), slog setup, wiring, graceful shutdown
├── internal/
│   ├── config/                  # Env-var configuration (LUNE_*), runtime NetworkConfig merge (server_settings["network_config"]), CIDR parsing & mode defaults, validation, DBPath/CertDir helpers
│   ├── edge/                    # HTTP server (:5863) & CertMagic ACME HTTPS (:443 + optional :80 under DNS-01), AEAD-only TLS 1.2+ ciphers, Host enforcement, per-IP/global connection cap (connlimit.go)
│   ├── security/                # Strict CSP/COOP/CORP headers, BodyLimitMiddleware, CompressMiddleware (gzip for API/SPA, skipping /stream/* & images), EdgeGuard (sharded per-IP rate limiter, auto-ban, allowlist/blocklist), hot-reloadable ClientIPResolver, AuthRateLimiter
│   ├── auth/                    # Argon2id passwords, pure-Go RFC 6238 TOTP 2FA + recovery codes (totp.go), web cookie & app token sessions, RFC 8628 device flow, user profiles/avatars/prefs, library ACLs, invites, watchlist/favorites, user admin, audit log
│   ├── store/                   # SQLite WAL split writer/reader pools, FTS5 search, embedded migrations, repositories
│   │   ├── migrations/          # Sequential embedded SQL migrations (001_init.sql .. 016_episode_provider_ids.sql)
│   │   ├── store.go             # Open/Close/Ping/Optimize/migrate (1 IMMEDIATE writer, 4-8 query_only readers, 64MB cache, 256MB mmap)
│   │   ├── fts.go               # Safe FTS5 prefix query sanitizer
│   │   ├── auth.go              # Users (including can_request/auto_approve_requests), sessions, device codes, audit events, server_settings
│   │   ├── users.go             # User preferences/avatars/profile updates, library ACLs, invites, watchlist & favorites
│   │   ├── requests.go          # Media requests CRUD, active-request & local-TMDB lookups, auto-available transitions
│   │   ├── media.go             # Libraries (movies/shows/anime/music/other) and media_items CRUD, batched scan upserts, FTS5 search (including original_title)
│   │   ├── series.go            # TV/Anime series grouping (with dual-audio & original_title), season/episode queries, next-episode lookup
│   │   └── playback.go          # Per-user playback_progress, Continue Watching, and Next Up queries
│   ├── library/                 # Library management, incremental scanner, filename parser, subtitles, watcher
│   │   ├── scanner.go           # Service, library CRUD, incremental/forced scan, single-item refresh, auto-marking matching requests available
│   │   ├── parser.go            # Movie/episode/anime parser (SxxExx, fansub `[Group] Title - 01`, ordinal seasons, absolute numbering, OVAs/Specials, anime films)
│   │   ├── series.go            # Series list, detail with progress, Up Next episode resolution
│   │   ├── subtitles.go         # Local sidecar subtitle discovery (.vtt, .srt, .ass, .ssa; language/forced/Signs & Songs detection)
│   │   └── watcher.go           # fsnotify debounced (5s) watcher + periodic fallback scan (for FUSE/Unraid)
│   ├── maintenance/             # Guarded periodic & manual housekeeping (orphaned artwork, stale transcode/sub cache, audit log pruning, SQLite optimize/checkpoint) with live-reconfigurable server_settings["maintenance_config"]
│   ├── metadata/                # Technical probing & descriptive enrichment
│   │   ├── probe.go             # ffprobe wrapper extracting container, duration, bitrate, video/audio/sub streams, MKV chapters (OP/ED/Recap/Preview), normalized audio languages
│   │   ├── nfo.go               # Local Kodi/Jellyfin-style .nfo XML parser (including `<originaltitle>`)
│   │   └── tmdb.go              # TMDB v3 enricher (direct key or proxy LUNE_TMDB_BASE_URL), Discover/target-details for Media Requests, anime-aware ranking & absolute-to-seasonal episode mapping + <DataDir>/artwork cache
│   ├── requests/                # Native Media Requests (TMDB Discover, approval workflow, optional Radarr/Sonarr v3 dispatch)
│   │   ├── config.go            # Server-settings persistence, env overrides, API key masking & preservation
│   │   ├── arr.go               # Zero-dependency Radarr v3 & Sonarr v3 HTTP client (test, lookup, add movie/series)
│   │   └── service.go           # Discovery annotation (library ACLs + active requests), request lifecycle, anime routing
│   ├── stream/                  # Playback negotiation, signed streaming, transcoding & watch progress
│   │   ├── service.go           # Capability negotiation (direct/remux/transcode), smart audio/sub selection (Sub/Dub), embedded & sidecar sub serving, Direct Play, HLS, chapters, active streams, progress
│   │   ├── token.go             # HMAC-SHA256 signed TokenClaims (6h TTL; secret persisted in server_settings)
│   │   ├── transcode.go         # FFmpeg HLS session manager (live-configurable concurrency limits & idle TTL via server_settings["streaming_config"]) + embedded subtitle WebVTT extractor/cache + HW accel detection (vaapi, videotoolbox, software)
│   │   └── subtitles.go         # Pure-Go SRT-to-WebVTT & ASS/SSA-to-WebVTT converters
│   ├── api/                     # Chi router + Huma v2 route registration & DTO mapping
│   │   ├── api.go               # NewHandler, middleware stack, /healthz, /api/v1/system/info (including live edge_mode, public_url, requests_enabled), SPA fallback
│   │   ├── auth_middleware.go   # Request metadata (IP/UA/HTTPS) & optional Bearer/lune_session cookie auth
│   │   ├── auth.go              # Setup, login, refresh, logout, me, sessions (+ revoke-others), RFC 8628 device flow, admin audit & alert dismissal
│   │   ├── network.go           # Live runtimeNetwork state & admin network config endpoints (GET/PUT /api/v1/admin/network/config) with live hot-reload for local/proxy/tailscale and restart-required signaling for ACME
│   │   ├── maintenance.go       # Admin housekeeping status, live config, and manual run endpoints (GET /api/v1/admin/maintenance, PUT /api/v1/admin/maintenance/config, POST /api/v1/admin/maintenance/run)
│   │   ├── users.go             # Self-service profile/password/prefs/avatars/watchlist/favorites (/api/v1/users/*) & admin user/session management (/api/v1/admin/users/*)
│   │   ├── invites.go           # Admin invite management (/api/v1/admin/invites) & public invite inspection/acceptance (/api/v1/invites/*)
│   │   ├── library.go           # Libraries, scans, items (+ genre/person/studio filtering, pre-play tracks/progress, /similar local matching), /api/v1/metadata/search (admin visual identify), refresh (with explicit tmdb_id binding), binary artwork endpoint (enforcing library ACLs)
│   │   ├── series.go            # Series list/detail (with synopsis/marks/watched_count), /api/v1/series/watched, /api/v1/series/refresh, and /api/v1/items/{id}/next
│   │   ├── requests.go          # Native Media Requests endpoints (/api/v1/requests/* and /api/v1/admin/requests/*)
│   │   └── playback.go          # Playback negotiation, signed direct-link/download (/api/v1/items/{id}/direct-link), progress, continue watching (/api/v1/playback/continue), next up (/api/v1/playback/next-up), active streams, admin streaming config (GET/PUT /api/v1/admin/streaming/config), /stream/*
│   ├── web/                     # Static SPA handler (NewSPAHandler) for built lune-web/dist
│   └── version/                 # Build-time Version, Commit, BuildDate (set via -ldflags)
├── deploy/
│   ├── docker-compose.yml       # Single-container deployment (ports 80/443, /data, /media:ro, optional /dev/dri)
│   └── unraid/lune.xml          # Unraid Community Applications template
├── docs/architecture.md         # Platform v1 blueprint & milestone roadmap
├── .github/workflows/           # ci.yml (vet, race test, static build) & release.yml (validated workflow_dispatch release, GitHub Release & multi-arch GHCR)
└── Dockerfile                   # Multi-stage static Go build + Debian bookworm-slim + FFmpeg runtime
```

---

## 4. Runtime Configuration (`internal/config`)

All configuration is environment-driven:

| Variable | Default | Purpose |
|---|---|---|
| `LUNE_DATA_DIR` | `./data` | Persistent directory for `lune.db`, `certs/`, `artwork/`, `avatars/`, `transcode/` |
| `LUNE_MEDIA_DIR` | `/media` | Default media root path |
| `LUNE_WEB_DIR` | *(empty)* | Path to built `lune-web/dist` directory to serve as SPA fallback |
| `LUNE_EDGE_MODE` | `local` | Access mode: `local` (LAN HTTP `:5863`, alias `dev`), `acme` (CertMagic HTTPS `:443` + `:80`), `proxy` (behind reverse proxy `:5863`, trusts Docker/RFC1918 CIDRs by default), or `tailscale` (Tailscale Serve `:5863`, trusts loopback + Tailscale CGNAT by default) |
| `LUNE_PUBLIC_URL` | *(empty)* | Optional canonical external URL (e.g. `https://lune.example.com`) for `Secure` cookies and invite links |
| `LUNE_LISTEN_ADDR` | `:5863` (`local`/`proxy`/`tailscale`) / `:443` (`acme`) | Primary HTTP/HTTPS bind address |
| `LUNE_HTTP_ADDR` | `:80` | ACME HTTP-01 challenge & HTTPS redirect listener (`acme` mode) |
| `LUNE_DOMAIN` | *(empty)* | Required in `acme` mode |
| `LUNE_ACME_EMAIL` | *(empty)* | ACME account email |
| `LUNE_ACME_STAGING` | `false` | Use Let's Encrypt staging CA |
| `LUNE_ACME_DNS_PROVIDER` | *(empty)* | Set to `cloudflare` for DNS-01 validation |
| `LUNE_CF_API_TOKEN` | *(empty)* | Required when `LUNE_ACME_DNS_PROVIDER=cloudflare` |
| `LUNE_TMDB_API_KEY` | *(empty)* | Optional TMDB v3 key for automatic metadata/artwork enrichment |
| `LUNE_TMDB_BASE_URL` | `https://api.themoviedb.org/3` | Optional custom TMDB proxy or base URL (allows empty API key) |
| `LUNE_REQUESTS_ENABLED` | *(unset)* | Optional boolean env override enabling/disabling Native Media Requests |
| `LUNE_RADARR_URL` | *(empty)* | Optional env override for primary Radarr v3 base URL |
| `LUNE_RADARR_API_KEY` | *(empty)* | Optional env override for primary Radarr v3 API key |
| `LUNE_SONARR_URL` | *(empty)* | Optional env override for primary Sonarr v3 base URL |
| `LUNE_SONARR_API_KEY` | *(empty)* | Optional env override for primary Sonarr v3 API key |
| `LUNE_SCAN_INTERVAL` | `30m` | Periodic fallback library scan interval (`0` disables periodic scans) |
| `LUNE_TRUSTED_PROXIES` | *(empty)* | Comma-separated IPs/CIDRs allowed to supply `X-Forwarded-For` |
| `LUNE_ADMIN_LAN_ONLY` | `false` | Restrict `/api/v1/admin/*` to loopback, RFC1918 LAN, Tailscale CGNAT (`100.64.0.0/10`), or `LUNE_IP_ALLOWLIST` |
| `LUNE_IP_ALLOWLIST` | *(empty)* | Comma-separated IPs/CIDRs exempt from edge rate limits/auto-bans and permitted under `LUNE_ADMIN_LAN_ONLY` |
| `LUNE_IP_BLOCKLIST` | *(empty)* | Comma-separated IPs/CIDRs immediately rejected with HTTP `403` at the edge |
| `LUNE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `LUNE_LOG_FORMAT` | `text` | `text` or `json` (`Dockerfile` sets `json`) |

---

## 5. Key Implementation Patterns & Invariants

### HTTP & OpenAPI Layer (`internal/api`, `internal/web`)
- **Middleware order** in `api.NewHandler`: `RequestID` -> `Recoverer` -> `security.BodyLimitMiddleware` -> `security.HeadersMiddleware` -> `security.EdgeGuardMiddleware` -> `security.CompressMiddleware` -> `requestMetaMiddleware` -> `adminLANOnlyMiddleware` -> `csrfMiddleware` -> `optionalAuthMiddleware`.
- **Auth enforcement:** `optionalAuthMiddleware` resolves `Authorization: Bearer <token>` or the `lune_session` HttpOnly cookie into request context. Handlers explicitly check `currentPrincipal(ctx)` for 401 and `user.Role != "admin"` for 403.
- **Typed JSON endpoints vs. raw Chi routes:** Use `huma.Register` for all JSON API endpoints so `/openapi.json` stays canonical. Use raw `chi.Router` routes only for binary/streaming responses (`/api/v1/items/{id}/artwork/{kind}`, `/api/v1/artwork/people/{file}`, `/api/v1/users/me/avatar`, `/api/v1/users/{id}/avatar`, `/stream/direct/{token}`, `/stream/hls/{token}/{file}`, `/stream/sub/{token}/{file}`).
- **SPA safety:** `web.NewSPAHandler` must never hijack `/api/`, `/stream/`, `/healthz`, `/docs`, or `/openapi.*`.
- **No write timeout:** `edge.Server` deliberately leaves `http.Server.WriteTimeout` at `0` so long-lived media streams are never prematurely terminated.

### Persistence (`internal/store`)
- **Split WAL pools & tuning:** Always use `s.db` (`MaxOpenConns(1)`, `_txlock=immediate`, 64 MiB cache, 256 MiB mmap, `temp_store=MEMORY`, `busy_timeout=10000`) for writes and `s.reader` (4–8 `query_only(ON)` conns, 32 MiB cache, 256 MiB mmap) for **all reads** (including hot-path `auth.go` session/user lookups) so concurrent browsing and artwork requests never serialize behind the single writer connection or scanner writes. `Store.Close()` runs `PRAGMA optimize` and a passive WAL checkpoint.
- **High-scale scanning & search:** Library scans flush items in batched prepared transactions (`UpsertMediaItemsBatch`, 100 items/tx) and fingerprint unchanged files via the covering index `idx_media_items_fingerprints`. Media and series search queries use the trigger-maintained `media_items_fts` FTS5 table (`unicode61 remove_diacritics 2`) via `buildFTSQuery`.
- **Migrations:** Add new schema changes as append-only numbered files in `internal/store/migrations/` (e.g., `010_*.sql`). Never edit already-applied migration files; `Store.migrate` applies them in lexical order inside transactions with `PRAGMA foreign_keys = OFF` outside the transaction followed by `PRAGMA foreign_key_check` so table-rebuilding migrations stay safe.

### Security & Auth (`internal/auth`, `internal/security`)
- Passwords are min 8 chars, hashed with Argon2id (PHC format) and timing-equalized on unknown usernames via `dummyHash`.
- Web logins set `HttpOnly`, `SameSite=Lax`, `Secure` (when `EdgeModeACME`, `https://` `PublicURL`, or trusted `X-Forwarded-Proto: https` via live `runtimeNetwork`) cookies; app logins return `access_token` + rotating `refresh_token`.
- Runtime network & access-mode settings persist in `server_settings["network_config"]` (loaded on startup in `cmd/lune/main.go` via `config.ApplyStoredNetworkConfig`). Switching among `local`, `proxy`, and `tailscale` via `PUT /api/v1/admin/network/config` hot-reloads `ClientIPResolver` and live `edge_mode`/`public_url` immediately; switching to/from or modifying `acme` persists and signals `restart_required: true`.
- Brute-force protection uses `AuthRateLimiter` (5 failures / 10m -> progressive lockout 30s..15m), returning HTTP `429` with `Retry-After`, and feeds lockouts into `security.EdgeGuard` (sharded per-IP rate limiter + auto-ban on repeated auth lockouts, forged `/stream/*` tokens, or unauthenticated API probing; managed via `/api/v1/admin/security/bans`). Invite inspection/acceptance is also rate-limited per IP.
- Pure-Go RFC 6238 TOTP two-factor authentication (`/api/v1/users/me/totp/{setup,enable,disable}` and `/api/v1/admin/users/{id}/totp` reset) issues 8 SHA-256-hashed one-time recovery codes; `POST /api/v1/auth/login` signals `X-Lune-TOTP-Required: true` when a TOTP-enabled account omits `totp_code`.
- Invite tokens are 256-bit random values stored only as SHA-256 hashes in `user_invites`; raw tokens are returned once at creation. Invite acceptance atomically validates expiry/revocation/use limits, creates the account with library grants, and increments `use_count`.
- **Per-user library ACLs:** Admins and users with `all_libraries=1` have unrestricted access. Restricted members (`all_libraries=0`) are constrained to `user_library_access` rows across libraries, items, artwork, series, playback negotiation, watch progress, Continue Watching, Next Up (`GET /api/v1/playback/next-up`), and watchlist/favorites. `UserPreferences` includes `combine_next_up` (defaults to `true`) for combining Next Up into Continue Watching on clients.
- User avatars (PNG/JPEG/WebP, max 2 MiB, sniffed via `http.DetectContentType`) are stored under `<DataDir>/avatars/` and cleaned up on avatar deletion or user removal.
- Security-relevant events (login, lockout, password changes, user admin actions, invite lifecycle, session revocation, device approvals) are recorded in `audit_events`.

### Library, Metadata & Streaming (`internal/library`, `internal/metadata`, `internal/stream`)
- Scans are incremental by file size, mtime, and `metadata_version` (`CurrentMetadataVersion`) unless `force=true`. When a remote enricher (TMDB) is configured, items indexed under an older `metadata_version` (or before TMDB was configured) are automatically backfilled on startup via `Watcher` (`BackfillStaleLibraries`) and on incremental scans, then skipped on subsequent scans once upgraded. `ScanSync` detaches from the HTTP request context (`context.WithoutCancel`) so a browser navigating away never aborts a scan or fails in-flight batches. Failures are recorded via `recordItemErr` into `ScanStatus.Errors`, `LastError`, and a bounded `ErrorSamples` list (`{path, message}`, max 10) returned by `/api/v1/libraries*` so the admin console can show *why* files failed.
- **First-class Anime (`kind = "anime"`):** Supports mixed Anime Series and standalone Anime Films in one library, fansub `[Group] Title - 01` / ordinal season / 4-digit absolute numbering / OVA & Specials (`Season 0`) parsing, anime-ranked TMDB matching (preferring Animation genre `16` + Japanese origin) with automatic absolute-to-seasonal episode mapping, MKV chapter probing (`intro`/`outro`/`recap`/`preview`), sidecar `.ass`/`.ssa` pure-Go WebVTT conversion, on-demand FFmpeg extraction & caching of embedded MKV text subtitles (`/stream/sub/{token}/embedded-{idx}.vtt`), and smart user-preference/anime Sub/Dub audio & subtitle auto-selection (auto-selecting Japanese audio + full subtitles when subbed, or English audio + forced Signs & Songs when dubbed).
- Metadata precedence during scan/refresh: filename parser (including embedded `{tmdb-...}` / `[tmdbid-...]` / `{imdb-tt...}` path tags) -> `ffprobe` streams & chapters -> local `.nfo` (including parent `tvshow.nfo` for `Season` subfolders) & local artwork -> previously pinned `tmdb_id` (preserved across force re-scans) -> optional TMDB enricher (direct ID lookup when `tmdb_id` is known, otherwise multi-signal candidate scoring across normalized title similarity, year proximity, vote/popularity confidence, and anime Animation/JP preference; artwork cached under `<DataDir>/artwork` with in-flight deduplication, atomic temp-file renames, and detached 15s singleflight downloads for lazy `FetchPersonPhoto` fetches so client cancellation does not abort cache fills; `/api/v1/artwork/people/{file}` maps genuine 404/invalid paths to `404` and transient upstream errors/timeouts to `503`/`504` with `Cache-Control: no-store`).
- Movie provider tags come from the filename and its own folder. For episodes, catalog `tmdb_id` and `imdb_id` identify the series; IDs from the episode sidecar are stored separately as `episode_tmdb_id` and `episode_imdb_id`. An IMDb ID alone does not replace an existing TMDB binding during a scan.
- Playback negotiation chooses the cheapest valid path: `direct` (HTTP Range `ServeContent`) -> `remux` (zero-transcode fMP4 HLS with `-c:v copy`, `-tag:v hvc1` for 4K HEVC, and `-c:a copy` when the selected audio track is already client-compatible fMP4 audio) -> `transcode` (fMP4 HLS with detected `videotoolbox` / `nvenc` / `vaapi` / `qsv` / `software` `libx264`, automatic HW-to-SW fallback on startup failure, `-pix_fmt yuv420p` for 10-bit sources, `libplacebo` Dolby Vision tone-mapping with automatic `zscale`/`tonemap` HDR-to-SDR fallback when available, 3s keyframe alignment, and optional FFmpeg `overlay` burn-in for PGS/VobSub/DVB bitmap subtitles). Selecting the primary (first) audio track avoids unnecessary remuxing, and 10-bit H.264 / unsupported HDR / Dolby Vision profiles automatically negotiate transcoding.
- HLS segment names are strictly validated (`^(index\.m3u8|init\.mp4|seg_\d{5}\.(m4s|ts))$`) to prevent path traversal, and embedded text subtitle extraction uses per-track singleflight deduplication with `-vn -an -dn`.
- Stream tokens expire after 6h and bind `sid`, `uid`, `iid`, and `mode`.
- Completing an item (`UpsertPlaybackProgress` with `completed = true`, automatically at ≥92% or explicitly marked watched) atomically removes it from the user's Watchlist while preserving Favorites.

---

## 6. Build, Test & Verification Commands

Run these before concluding any backend change (matches `.github/workflows/ci.yml`):

```bash
go vet ./...
go test -race -count=1 ./...
CGO_ENABLED=0 go build -trimpath -o /tmp/lune ./cmd/lune
```

Local development server:
```bash
go run ./cmd/lune
# Or serving a built lune-web bundle:
LUNE_WEB_DIR=../lune-web/dist go run ./cmd/lune
```

---

## 7. Keeping Documentation Up to Date

When implementing new functionality in `lune`, check and update:
1. **This file (`AGENTS.md`):** Update the package layout, environment variable table, invariants, or verification commands whenever they change.
2. **`docs/architecture.md`:** Keep the high-level blueprint and milestone progress aligned with significant architectural additions.
3. **`deploy/docker-compose.yml` & `deploy/unraid/lune.xml`:** Add any new user-facing `LUNE_*` environment variables or volume/device requirements.
4. **Cross-repo impact:** If you add or modify `/api/v1/*` routes or DTO fields, note or apply matching updates in `../lune-web/src/lib/api.ts` and `../lune-web/AGENTS.md`. Make those edits on the same-named branch in `../lune-web` (or in the matching worktree when that checkout has uncommitted changes from another task), never on whatever branch happens to be checked out there.
