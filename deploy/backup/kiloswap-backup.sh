#!/usr/bin/env bash
# Encrypted copy of Kiloswap's state to the router's disk (as the host's
# key backup does):
#   kiloswap-backup.sh full     daily (timer): database + seed.dat +
#                               boltz.conf + the guard's ledger
#   kiloswap-backup.sh change   after a swap changed state (the bot runs it,
#                               at most every 2 minutes): the database
# Without the database a user who lost their browser cannot rescue with
# their rescue key; with it the service comes back on another machine.
# gpg AES256, passphrase in $PASS (600). Run as root.
set -euo pipefail
MODE=${1:-full}
COMPOSE_DIR=${COMPOSE_DIR:?the deploy directory}
set -a; . "$COMPOSE_DIR/.env"; set +a
R=${KILOSWAP_ROOT:?}
DEST=${DEST:-/mnt/router/KILOSWAP}
PASS=${PASS:-/root/kiloswap-backup/passphrase}
KEEP_FULL_DAYS=60
KEEP_CHANGE_DAYS=7
ts=$(date -u +%Y%m%dT%H%M%SZ)
mountpoint -q "${DEST%/*}" || { echo "router share not mounted" >&2; exit 2; }
mkdir -p "$DEST/full" "$DEST/change"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
# A consistent dump (one transaction), custom format for pg_restore
docker compose -f "$COMPOSE_DIR/docker-compose.yml" --project-directory "$COMPOSE_DIR" \
	exec -T postgres pg_dump -U boltz -d boltz -Fc >"$tmp/boltz.pgdump"
if [ "$MODE" = full ]; then
	cp "$R/boltz/seed.dat" "$R/boltz/boltz.conf" "$tmp/"
	cp "$R/guard/ledger.json" "$tmp/" 2>/dev/null || true
fi
out="$DEST/$MODE/kiloswap-$MODE-$ts.tar.gpg"
tar -C "$tmp" -cf - . | gpg --batch --yes --pinentry-mode loopback --passphrase-file "$PASS" \
	--symmetric --cipher-algo AES256 -o "$out.part"
mv "$out.part" "$out"
# It must decrypt and list before it counts
gpg --batch --pinentry-mode loopback --passphrase-file "$PASS" -d "$out" 2>/dev/null | tar -tf - >/dev/null
find "$DEST/full" -name '*.tar.gpg' -mtime +$KEEP_FULL_DAYS -delete
find "$DEST/change" -name '*.tar.gpg' -mtime +$KEEP_CHANGE_DAYS -delete
echo "$(date -u +%FT%TZ) $MODE ok $(stat -c %s "$out") bytes" >>"$DEST/LAST"
