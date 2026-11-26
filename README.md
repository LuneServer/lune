# Lune

*Your universe. Your media.*

Lune is a self-hosted media platform designed to feel like calm, precision software — with an integrated HTTPS/ACME edge, built-in security, and a clean media engine that replaces the need to stitch together separate reverse proxies and media servers.

See [docs/architecture.md](docs/architecture.md) for the platform blueprint and roadmap.

## Quickstart (Local Dev)

```bash
go run ./cmd/lune
```

The server starts on `http://localhost:5863` (`LUNE_EDGE_MODE=local`):

- `GET /healthz` — liveness & SQLite readiness
- `GET /api/v1/system/info` — public server discovery metadata
- `GET /openapi.json` — canonical OpenAPI 3.1 contract
- `GET /docs` — interactive API documentation

## Production (Embedded ACME Edge)

```bash
LUNE_EDGE_MODE=acme \
LUNE_DOMAIN=lune.example.com \
LUNE_ACME_EMAIL=admin@example.com \
LUNE_DATA_DIR=/data \
lune
```

Or use [deploy/docker-compose.yml](deploy/docker-compose.yml).
