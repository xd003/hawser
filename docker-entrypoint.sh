#!/bin/sh
set -e

# Hawser Docker Entrypoint
#
# PUID/PGID are intentionally not baked into the image: when neither is set the
# container keeps running as root (backward compatible). When either is set,
# Hawser drops to that UID/GID; the unset one defaults to 1001. PUID=0 keeps
# root. Starting the container with a `user:` directive / --user skips all of
# this and runs Hawser as that user untouched.

HAWSER_BIN=/usr/local/bin/hawser
SOCKET_PATH="${DOCKER_SOCKET:-/var/run/docker.sock}"
STACKS_DIR="${STACKS_DIR:-/data/stacks}"
RUN_USER=hawser

if [ -n "${PUID+x}" ] || [ -n "${PGID+x}" ]; then
    PUID_PGID_SET=true
else
    PUID_PGID_SET=false
fi
PUID=${PUID:-1001}
PGID=${PGID:-1001}

fail() {
    echo "ERROR: $*" >&2
    exit 1
}

# === Not root (user: directive / --user) or PUID/PGID not requested ===
if [ "$(id -u)" != "0" ]; then
    echo "Running as user $(id -u):$(id -g) (set via container user directive)"
    if [ "$PUID_PGID_SET" = "true" ]; then
        echo "NOTE: PUID/PGID ignored - container was not started as root"
    fi
    exec "$HAWSER_BIN" "$@"
fi

if [ "$PUID_PGID_SET" = "false" ] || [ "$PUID" = "0" ]; then
    echo "Running as root user"
    exec "$HAWSER_BIN" "$@"
fi

case "$PUID$PGID" in
    ''|*[!0-9]*) fail "PUID and PGID must be numeric (PUID=$PUID PGID=$PGID)" ;;
esac

# === User Setup ===
# The container filesystem survives restarts, so drop any user left over from a
# previous start with different IDs before recreating it.
deluser "$RUN_USER" >/dev/null 2>&1 || true
delgroup "$RUN_USER" >/dev/null 2>&1 || true

EXISTING=$(awk -F: -v uid="$PUID" '$3 == uid { print $1 }' /etc/passwd)
[ -z "$EXISTING" ] || fail "UID $PUID is already used by '$EXISTING' in the image; choose another PUID"

TARGET_GROUP=$(awk -F: -v gid="$PGID" '$3 == gid { print $1 }' /etc/group)
if [ -z "$TARGET_GROUP" ]; then
    TARGET_GROUP="$RUN_USER"
    addgroup -g "$PGID" "$TARGET_GROUP" || fail "cannot create group $PGID (read-only root filesystem?)"
fi
adduser -u "$PUID" -G "$TARGET_GROUP" -h "/home/$RUN_USER" -D "$RUN_USER" \
    || fail "cannot create user $PUID (read-only root filesystem?)"
echo "Configured user $RUN_USER with PUID=$PUID PGID=$PGID"

# Docker CLI / buildx keep config under $HOME.
mkdir -p "/home/$RUN_USER"
chown "$PUID:$PGID" "/home/$RUN_USER"
export HOME="/home/$RUN_USER"

# === Directory Ownership ===
# Hawser must own STACKS_DIR (and its stack-binding registry, which it refuses
# to use unless owned by its own uid). Only entries owned by root are re-owned:
# data written by stack containers under their own UID (e.g. ./postgresql
# mounted into a postgres container) is left alone.
mkdir -p "$STACKS_DIR" || fail "cannot create STACKS_DIR $STACKS_DIR"
chown "$PUID:$PGID" "$STACKS_DIR" 2>/dev/null \
    || echo "WARNING: cannot chown $STACKS_DIR (NFS/root-squash?); Hawser may be unable to write to it"
find "$STACKS_DIR" -xdev -user 0 -exec chown -h "$PUID:$PGID" {} + 2>/dev/null || true

# === Docker Socket Access ===
if [ -S "$SOCKET_PATH" ]; then
    if ! su-exec "$RUN_USER" test -r "$SOCKET_PATH" 2>/dev/null; then
        SOCKET_GID=$(stat -c '%g' "$SOCKET_PATH" 2>/dev/null || echo "")
        if [ -n "$SOCKET_GID" ]; then
            DOCKER_GROUP=$(awk -F: -v gid="$SOCKET_GID" '$3 == gid { print $1 }' /etc/group)
            if [ -z "$DOCKER_GROUP" ]; then
                DOCKER_GROUP=docker
                addgroup -g "$SOCKET_GID" "$DOCKER_GROUP" 2>/dev/null || true
            fi
            addgroup "$RUN_USER" "$DOCKER_GROUP" 2>/dev/null || true
        fi
        if su-exec "$RUN_USER" test -r "$SOCKET_PATH" 2>/dev/null; then
            echo "Docker socket accessible at $SOCKET_PATH"
        else
            echo "WARNING: Docker socket not accessible to $RUN_USER; try --group-add ${SOCKET_GID:-<socket gid>}"
        fi
    fi
fi

echo "Running as user: $RUN_USER ($PUID:$PGID)"
exec su-exec "$RUN_USER" "$HAWSER_BIN" "$@"
