#!/usr/bin/env bash
# Bakes Kiloswap's two macaroons on the operator's lnd (running, unlocked).
# Nothing restarts. Each gets its own root key, so either can be revoked
# alone (lncli deletemacaroonid N).
#
# To re-bake one of them with new permissions (e.g. the guard's, when it
# needs a new call), on a root key id nobody uses yet (lncli listmacaroonids):
#   ONLY=kiloswap-guard.macaroon FORCE=1 GUARD_ROOT_KEY_ID=33 ./bake-macaroons.sh
#   docker compose up -d --force-recreate guard
# and then, once the guard runs on it, revoke the old id
# (lncli deletemacaroonid 32).
#
#   kiloswap.macaroon        the backend: the calls it makes, all of them
#                            with the custom caveat kiloswap=v1, so lnd hands
#                            each one to the guard and refuses them all while
#                            the guard is not registered
#   kiloswap-guard.macaroon  the guard: register as middleware, read the
#                            wallet balance, look up and decode invoices,
#                            settle and cancel Kiloswap's hold invoices,
#                            follow its payments, new addresses for its
#                            claims and refunds, list the wallet's coins (to
#                            name the ones a lockup spends)
set -euo pipefail
cd "$(dirname "$0")"
set -a; . ./.env; set +a
R=${KILOSWAP_ROOT:?}
LNCLI=${LNCLI:-docker exec ${LND_HOST:?set LND_HOST in .env} lncli --network=mainnet}
KS_ID=${KS_ROOT_KEY_ID:-31}
GUARD_ID=${GUARD_ROOT_KEY_ID:-32}
backend=(
	uri:/lnrpc.Lightning/GetInfo uri:/lnrpc.Lightning/AddInvoice uri:/lnrpc.Lightning/LookupInvoice
	uri:/lnrpc.Lightning/DecodePayReq uri:/lnrpc.Lightning/GetNodeInfo uri:/lnrpc.Lightning/GetChanInfo
	uri:/lnrpc.Lightning/QueryRoutes uri:/lnrpc.Lightning/ListChannels
	uri:/lnrpc.Lightning/ListPeers uri:/lnrpc.Lightning/WalletBalance
	uri:/lnrpc.Lightning/SubscribePeerEvents uri:/lnrpc.Lightning/SubscribeChannelEvents
	uri:/lnrpc.Lightning/ExportAllChannelBackups uri:/lnrpc.Lightning/SubscribeChannelBackups
	uri:/routerrpc.Router/SendPaymentV2 uri:/routerrpc.Router/TrackPaymentV2
	uri:/invoicesrpc.Invoices/AddHoldInvoice
	uri:/invoicesrpc.Invoices/CancelInvoice uri:/invoicesrpc.Invoices/SettleInvoice
	uri:/invoicesrpc.Invoices/SubscribeSingleInvoice
	uri:/lnrpc.Lightning/NewAddress uri:/lnrpc.Lightning/SendCoins uri:/lnrpc.Lightning/GetTransactions
)
guard=(
	uri:/lnrpc.Lightning/RegisterRPCMiddleware uri:/lnrpc.Lightning/WalletBalance
	uri:/lnrpc.Lightning/LookupInvoice uri:/lnrpc.Lightning/DecodePayReq
	uri:/invoicesrpc.Invoices/SettleInvoice uri:/invoicesrpc.Invoices/CancelInvoice
	uri:/routerrpc.Router/TrackPaymentV2 uri:/lnrpc.Lightning/NewAddress
	uri:/lnrpc.Lightning/GetTransactions uri:/lnrpc.Lightning/ListUnspent
)
bake() { # NAME ROOT_KEY_ID OWNER ARGS...
	local name=$1 key=$2 owner=$3; shift 3
	if [ -n "${ONLY:-}" ] && [ "$ONLY" != "$name" ]; then return; fi
	if [ -s "$R/macaroons/$name" ] && [ "${FORCE:-}" != 1 ]; then echo "keep $name"; return; fi
	local hex
	hex=$($LNCLI bakemacaroon --root_key_id "$key" "$@")
	printf %s "$hex" | xxd -r -p >"$R/macaroons/$name.tmp"
	sudo chown "$owner" "$R/macaroons/$name.tmp"
	sudo chmod 400 "$R/macaroons/$name.tmp"
	sudo mv "$R/macaroons/$name.tmp" "$R/macaroons/$name"
	echo "baked $name (root key $key)"
}
bake kiloswap.macaroon "$KS_ID" 0:0 --custom_caveat_name kiloswap --custom_caveat_condition v1 "${backend[@]}"
bake kiloswap-guard.macaroon "$GUARD_ID" 65532:65532 "${guard[@]}"
