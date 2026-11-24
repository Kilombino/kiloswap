#!/usr/bin/env bash
# One-time setup on the host (idempotent): directories, secrets, the
# backend's config from boltz.conf.template, the node's rpcauth line to add.
# Touches neither the node nor lnd: it prints what to change there.
set -euo pipefail
cd "$(dirname "$0")"
[ -f .env ] || { echo "copy .env.example to .env first" >&2; exit 1; }
set -a; . ./.env; set +a
R=${KILOSWAP_ROOT:?}
umask 077
mkdir -p "$R"/{secrets,postgres,shim,boltz,guard,macaroons,webapp,backups}
gen() { [ -s "$R/secrets/$1" ] || openssl rand -hex 24 >"$R/secrets/$1"; }
gen postgres_password
gen guard_db_password
gen node_rpc_password
pg=$(cat "$R/secrets/postgres_password")
printf 'postgres://kiloswap_guard:%s@postgres:5432/boltz?sslmode=disable\n' \
	"$(cat "$R/secrets/guard_db_password")" >"$R/secrets/guard_db_url"
rpc=$(cat "$R/secrets/node_rpc_password")
sed -i "s|^NODE_RPC_PASSWORD=.*|NODE_RPC_PASSWORD=$rpc|" .env
if [ ! -s "$R/boltz/boltz.conf" ]; then
	sed -e "s|@POSTGRES_PASSWORD@|$pg|" -e "s|@NODE_RPC_PASSWORD@|$rpc|" \
		-e "s|@LND_HOST@|${LND_HOST:-lnd-blake2b}|" boltz.conf.template >"$R/boltz/boltz.conf"
fi
chmod 600 .env "$R"/secrets/* "$R/boltz/boltz.conf"
# The guard runs as distroless's nonroot (65532): its state and secrets
sudo chown -R 65532:65532 "$R/guard"
sudo chown 65532:65532 "$R/secrets/guard_db_url" "$R/secrets/node_rpc_password"
# rpcauth line for the node (Core's share/rpcauth: HMAC-SHA256 with a salt)
salt=$(openssl rand -hex 16)
hmac=$(printf %s "$rpc" | openssl dgst -sha256 -hmac "$salt" | awk '{print $2}')
cat <<MSG
Secrets in $R/secrets (600). Backend config: $R/boltz/boltz.conf.

For the node's bitcoin.conf (in the maintenance window, see RUNBOOK.md):
rpcauth=kiloswap:$salt\$$hmac
MSG
