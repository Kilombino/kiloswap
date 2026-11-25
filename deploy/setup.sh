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
mkdir -p "$R"/{secrets,postgres,shim,boltz,guard,macaroons,webapp,backups} "$R/secrets/lnd-proxy"
# lnd-proxy's own certificate (the backend trusts it for lnd-proxy:10009)
if [ ! -s "$R/secrets/lnd-proxy/proxy.crt" ]; then
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
		-subj /CN=lnd-proxy -addext subjectAltName=DNS:lnd-proxy \
		-keyout "$R/secrets/lnd-proxy/proxy.key" -out "$R/secrets/lnd-proxy/proxy.crt" 2>/dev/null
	chmod 640 "$R/secrets/lnd-proxy/proxy.key"
fi
gen() { [ -s "$R/secrets/$1" ] || openssl rand -hex 24 >"$R/secrets/$1"; }
gen postgres_password
gen postgres_admin_password
gen guard_db_password
gen node_rpc_password
gen guard_rpc_password
gen guard_api_token
cp "$R/secrets/guard_api_token" "$R/secrets/guard_api_token_backend"
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
# The swaps' server key: the guard's alone. The backend gets the extended
# public key at m/0/0 and a fresh seed.dat of its own (the sidecar wants one;
# it signs nothing with it). Moving an existing backend's seed.dat here is a
# one-time step (RUNBOOK): it must leave the backend's directory.
if [ ! -s "$R/secrets/swap_seed" ]; then
	echo "put the swaps' mnemonic in $R/secrets/swap_seed (a new install: docker run --rm kiloswap/guard newseed)" >&2
	exit 1
fi
sudo chown 65532:65532 "$R/secrets/swap_seed"
sudo chmod 400 "$R/secrets/swap_seed"
sudo docker run --rm -v "$R/secrets/swap_seed:/s:ro" -e SWAP_MNEMONIC_FILE=/s kiloswap/guard:${GUARD_TAG:-dev} xpub \
	| sudo tee "$R/secrets/swap_xpub" >/dev/null
# The guard runs as distroless's nonroot (65532): its state and secrets;
# the backend (root in its image) reads its own copy of the API token
sudo chown -R 65532:65532 "$R/guard"
sudo chown 65532:65532 "$R/secrets/guard_db_url" "$R/secrets/guard_rpc_password" "$R/secrets/guard_api_token"
# rpcauth lines for the node (Core's share/rpcauth: HMAC-SHA256 with a salt)
auth() {
	local salt hmac
	salt=$(openssl rand -hex 16)
	hmac=$(printf %s "$2" | openssl dgst -sha256 -hmac "$salt" | awk '{print $2}')
	echo "rpcauth=$1:$salt\$$hmac"
}
cat <<MSG
Secrets in $R/secrets (600). Backend config: $R/boltz/boltz.conf.

For the node's bitcoin.conf (in the maintenance window, see RUNBOOK.md):
$(auth kiloswap "$rpc")
$(auth kiloswap-guard "$(cat "$R/secrets/guard_rpc_password")")
MSG
