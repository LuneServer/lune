<div align="center">
  <br />
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/LuneServer/lune-brand/main/svg/lune-lockup-dark.svg" />
    <source media="(prefers-color-scheme: light)" srcset="https://raw.githubusercontent.com/LuneServer/lune-brand/main/svg/lune-lockup-light.svg" />
    <img src="https://raw.githubusercontent.com/LuneServer/lune-brand/main/svg/lune-lockup-dark.svg" alt="Lune" width="216" />
  </picture>

  <p><strong>Your universe. Your media.</strong></p>
  <p>A fast, modern, self-hosted media server for your movies, TV shows, and anime — packaged as a single container with automatic HTTPS, hardware transcoding, and built-in media requests.</p>

  <p>
    <a href="https://github.com/LuneServer/lune/releases"><img src="https://img.shields.io/github/v/release/LuneServer/lune?include_prereleases&style=flat-square&color=141417&labelColor=09090B" alt="Release" height="20" /></a>
    <a href="https://github.com/LuneServer/lune/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/LuneServer/lune/ci.yml?branch=main&style=flat-square&label=CI&labelColor=09090B" alt="CI" height="20" /></a>
    <a href="https://github.com/LuneServer/lune/pkgs/container/lune"><img src="https://img.shields.io/badge/ghcr.io-luneserver%2Flune-64D2FF?style=flat-square&logo=docker&logoColor=F5F5F7&labelColor=09090B" alt="GHCR Image" height="20" /></a>
    <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.26-00ADD8?style=flat-square&logo=go&logoColor=F5F5F7&labelColor=09090B" alt="Go 1.26" height="20" /></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/License-AGPL--3.0-F5F5F7?style=flat-square&labelColor=09090B" alt="AGPL-3.0 License" height="20" /></a>
  </p>

  <p>
    <a href="https://github.com/LuneServer/lune-web"><strong>Web Client</strong></a> ·
    <a href="https://github.com/LuneServer/lune-brand"><strong>Brand &amp; Tokens</strong></a> ·
    <a href="https://github.com/LuneServer/unraid-templates"><strong>Unraid Template</strong></a> ·
    <a href="https://github.com/LuneServer/lune/issues"><strong>Issues</strong></a>
  </p>
</div>

> [!WARNING]
> **Early development:** Lune is under active development and may be unstable. New versions are released frequently as the project evolves. If you're a developer who wants to help shape it, contributions and feedback are very welcome.

---

## Why Lune?

- **One Container, Everything Included** — No separate database, cache, or reverse proxy to set up. Drop in one container, point it at your media folders, and start watching.
- **Smooth Playback & Hardware Transcoding** — Direct Plays and Direct Streams whenever possible for zero quality loss, and accelerates transcoding on Intel QuickSync, AMD, NVIDIA, and Apple GPUs — including automatic HDR & Dolby Vision tone-mapping.
- **Fast Subtitles Out of the Box** — Embedded and external subtitles load in seconds without waiting for huge 4K files to scan, with support for SRT, ASS/SSA, WebVTT, and PGS/VobSub burn-in.
- **Built for Anime Too** — Mix anime series and films in one library, parse fansub and absolute-numbered episodes cleanly, skip openings/endings automatically, and switch between Subbed and Dubbed in one click.
- **Built-In HTTPS & Security** — Automatic Let's Encrypt HTTPS (including Cloudflare DNS-01), or run effortlessly behind your own reverse proxy or Tailscale. Includes invite links, per-library permissions, two-factor authentication (2FA), and brute-force protection.
- **Built-In Media Requests** — Let friends and family browse and request movies or shows directly inside Lune, with optional Radarr and Sonarr integration.

---

## Quick Start

### Docker Compose

Use the included [deploy/docker-compose.yml](deploy/docker-compose.yml):

```yaml
services:
  lune:
    image: ghcr.io/luneserver/lune:latest
    container_name: lune
    restart: unless-stopped
    ports:
      - "5863:5863"
      # For built-in automatic HTTPS (LUNE_EDGE_MODE=acme), expose 80 & 443 instead:
      # - "80:80"
      # - "443:443"
    environment:
      PUID: 1000            # Use 99 on Unraid
      PGID: 1000            # Use 100 on Unraid
      UMASK: "002"
      LUNE_EDGE_MODE: local # local | acme | proxy | tailscale
    volumes:
      - ./data:/data
      - /path/to/media:/media:ro
    # Optional: pass through your Intel/AMD iGPU for hardware transcoding
    devices:
      - /dev/dri:/dev/dri
```

```bash
docker compose up -d
```

Open **`http://localhost:5863`** in your browser to create your admin account and add your libraries.

### Unraid

Install directly from **Unraid Community Applications** via [LuneServer/unraid-templates](https://github.com/LuneServer/unraid-templates). Pass `/dev/dri` as a device for Intel/AMD iGPU transcoding — Lune handles GPU permissions automatically.

---

## Common Settings

Most settings (networking, transcoding limits, Radarr/Sonarr, maintenance) can be configured right in the **Admin Console**. A few environment variables are useful at container startup:

| Variable | Default | Description |
|---|---|---|
| `LUNE_EDGE_MODE` | `local` | How you access Lune: `local` (LAN `:5863`), `acme` (built-in HTTPS), `proxy`, or `tailscale` |
| `LUNE_PUBLIC_URL` | *(empty)* | Your external URL (e.g. `https://lune.example.com`) for invite links and secure cookies |
| `LUNE_DOMAIN` | *(empty)* | Your domain name when using built-in HTTPS (`LUNE_EDGE_MODE=acme`) |
| `LUNE_HW_ACCEL` | `auto` | Hardware transcoding backend (`auto`, `vaapi`, `qsv`, `nvenc`, `videotoolbox`, or `software`) |
| `LUNE_VAAPI_DEVICE` | *(auto)* | Specific GPU render node (e.g. `/dev/dri/renderD128`) if you have multiple GPUs |
| `PUID` / `PGID` / `UMASK` | `1000` / `1000` / `002` | File ownership inside `/data` (use `99` / `100` on Unraid) |

---

## Local Development

Requires **Go 1.26+** and `ffmpeg`/`ffprobe`:

```bash
# Start the server on http://localhost:5863
go run ./cmd/lune

# Serve a local lune-web build
LUNE_WEB_DIR=../lune-web/dist go run ./cmd/lune

# Run tests
go vet ./... && go test -race -count=1 ./...
```

---

## License

Licensed under the [GNU Affero General Public License v3.0](LICENSE). Third-party notices and licenses are listed in [THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md).
