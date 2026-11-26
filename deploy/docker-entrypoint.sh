#!/bin/sh
set -eu

# Allow passing flags directly (e.g. `docker run ghcr.io/luneserver/lune --version`)
if [ "$#" -eq 0 ] || [ "${1#-}" != "$1" ]; then
  set -- /usr/local/bin/lune "$@"
fi

# Apply UMASK if set (default 002 for friendly shared-group permissions on Unraid/NAS)
umask "${UMASK:-002}"

# If the container was started with an explicit non-root `--user` / `user:` directive,
# skip privilege dropping and ownership changes.
if [ "$(id -u)" -ne 0 ]; then
  exec "$@"
fi

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"
DATA_DIR="${LUNE_DATA_DIR:-/data}"

# Validate PUID/PGID are non-negative integers
case "$PUID" in
  ''|*[!0-9]*) echo "lune entrypoint: invalid PUID '$PUID'" >&2; exit 1 ;;
esac
case "$PGID" in
  ''|*[!0-9]*) echo "lune entrypoint: invalid PGID '$PGID'" >&2; exit 1 ;;
esac

# If explicitly configured to run as root (PUID=0), execute directly.
if [ "$PUID" -eq 0 ]; then
  mkdir -p "$DATA_DIR"
  exec "$@"
fi

# Remap the built-in lune group/user to the requested PGID/PUID.
# -o permits matching pre-existing Debian IDs (e.g. GID 100 'users' on Unraid).
groupmod -o -g "$PGID" lune
usermod -o -u "$PUID" -g "$PGID" -d "$DATA_DIR" lune

# If /dev/dri is passed through for VAAPI/QuickSync hardware transcoding,
# grant the lune user supplementary group access to the host GPU device nodes.
if [ -d /dev/dri ]; then
  for dev in /dev/dri/renderD* /dev/dri/card*; do
    [ -e "$dev" ] || continue
    dev_gid="$(stat -c '%g' "$dev" 2>/dev/null || true)"
    [ -n "$dev_gid" ] && [ "$dev_gid" != "0" ] || continue
    grp_name="$(getent group "$dev_gid" | cut -d: -f1 | head -n 1 || true)"
    if [ -z "$grp_name" ]; then
      grp_name="lune-dri-${dev_gid}"
      groupadd -o -g "$dev_gid" "$grp_name"
    fi
    usermod -aG "$grp_name" lune
  done
fi

# Ensure the persistent data directory exists and is owned by PUID:PGID.
# Only chown entries whose ownership differs so restarts with large artwork caches stay fast.
mkdir -p "$DATA_DIR"
find "$DATA_DIR" \( ! -uid "$PUID" -o ! -gid "$PGID" \) -exec chown -h "${PUID}:${PGID}" {} + \
  || echo "lune entrypoint: warning: could not update ownership on '$DATA_DIR' to ${PUID}:${PGID}" >&2

export HOME="$DATA_DIR"
exec gosu lune "$@"
