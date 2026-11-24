#!/usr/bin/env bash
# The guard end to end on the regtest lab (paulscode/lightning-fork-swap's
# deploy/regtest with the Kiloswap backend), with the real guard in
# lnd-swap's middleware chain:
#   1. the backend switched to the guarded macaroon; every e2e scenario
#      (both directions, refunds, rescues, ~1 BTC swaps) must still pass;
#   2. a backend taken over: direct lnd calls with the backend's macaroon
#      (and with a too-wide one) that the guard must refuse;
#   3. the guard stopped: lnd refuses the backend's macaroon outright.
#
#   LAB=/path/to/lab guard/test/lab.sh            (the lab up: kiloswap-lab start)
#   KEEP=1 ...  leaves the guard running and the backend on the guarded macaroon
set -euo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
LAB=${LAB:?LAB=the lab directory}
R=$LAB/src/swap/deploy/regtest
NET=${NET:-kiloswap-lab_default}
work=$(mktemp -d)
GUARD=kiloswap-lab-guard
cd "$R"

pass=0; failed=0
log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok() { printf '  ok   %s\n' "$*"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$*" >&2; failed=$((failed + 1)); }
K() { docker compose exec -T knots bitcoin-cli -regtest -rpcuser=lab -rpcpassword=lab "$@"; }
LS() { docker compose exec -T lnd-swap lncli --network=regtest "$@" </dev/null; }
LU() { docker compose exec -T lnd-user lncli --network=regtest "$@" </dev/null; }
KS() { LS --macaroonpath /root/.lnd/kiloswap.macaroon "$@"; }
WIDE() { LS --macaroonpath /root/.lnd/kiloswap-wide.macaroon "$@"; }
psql() { docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -q -U boltz -d boltz "$@"; }
# refused NAME CMD...: the call must fail, by the guard (or lnd's permission
# check when the macaroon lacks the method)
refused() {
	local name=$1; shift
	local out
	if out=$("$@" 2>&1); then bad "$name: went through: ${out:0:200}"; return; fi
	if grep -q -E 'kiloswap guard|permission denied|middleware|not allowed' <<<"$out"; then
		ok "$name refused: $(grep -o -E 'kiloswap guard: [^"]*|permission denied|middleware[^"]*' <<<"$out" | head -1 | cut -c1-120)"
	else
		bad "$name failed for another reason: ${out:0:200}"
	fi
}
allowed() {
	local name=$1; shift
	local out
	if out=$("$@" 2>&1); then ok "$name allowed"; else bad "$name refused: ${out:0:200}"; fi
}
restore() {
	docker rm -f $GUARD >/dev/null 2>&1 || true
	if [ -f "$work/boltz.conf.orig" ]; then
		cp "$work/boltz.conf.orig" boltz.conf
		docker compose restart boltz >/dev/null 2>&1 || true
	fi
	rm -rf "$work"
}
trap '[ -n "${KEEP:-}" ] || restore' EXIT

log "guard binary"
( cd "$here" && CGO_ENABLED=0 go build -trimpath -o "$work/kiloswap-guard" ./cmd/kiloswap-guard )
ok "built"

log "macaroons"
BOLTZ_URIS=(
	uri:/lnrpc.Lightning/GetInfo uri:/lnrpc.Lightning/AddInvoice uri:/lnrpc.Lightning/LookupInvoice
	uri:/lnrpc.Lightning/DecodePayReq uri:/lnrpc.Lightning/GetNodeInfo uri:/lnrpc.Lightning/GetChanInfo
	uri:/lnrpc.Lightning/DescribeGraph uri:/lnrpc.Lightning/QueryRoutes uri:/lnrpc.Lightning/ListChannels
	uri:/lnrpc.Lightning/ListPeers uri:/lnrpc.Lightning/WalletBalance
	uri:/lnrpc.Lightning/SubscribePeerEvents uri:/lnrpc.Lightning/SubscribeChannelEvents
	uri:/lnrpc.Lightning/ExportAllChannelBackups uri:/lnrpc.Lightning/SubscribeChannelBackups
	uri:/routerrpc.Router/SendPaymentV2 uri:/routerrpc.Router/TrackPaymentV2
	uri:/routerrpc.Router/ResetMissionControl uri:/invoicesrpc.Invoices/AddHoldInvoice
	uri:/invoicesrpc.Invoices/CancelInvoice uri:/invoicesrpc.Invoices/SettleInvoice
	uri:/invoicesrpc.Invoices/SubscribeSingleInvoice
	uri:/lnrpc.Lightning/NewAddress uri:/lnrpc.Lightning/SendCoins uri:/lnrpc.Lightning/GetTransactions
)
LS bakemacaroon --custom_caveat_name kiloswap --custom_caveat_condition v1 \
	--save_to /root/.lnd/kiloswap.macaroon "${BOLTZ_URIS[@]}" >/dev/null
# Too wide on purpose: the guard alone must stop these
LS bakemacaroon --custom_caveat_name kiloswap --custom_caveat_condition v1 \
	--save_to /root/.lnd/kiloswap-wide.macaroon "${BOLTZ_URIS[@]}" \
	uri:/lnrpc.Lightning/OpenChannelSync uri:/lnrpc.Lightning/OpenChannel uri:/lnrpc.Lightning/SendMany \
	uri:/lnrpc.Lightning/CloseChannel uri:/routerrpc.Router/SendToRouteV2 uri:/lnrpc.Lightning/ConnectPeer \
	uri:/walletrpc.WalletKit/SendOutputs uri:/lnrpc.Lightning/BakeMacaroon >/dev/null
LS bakemacaroon --save_to /root/.lnd/kiloswap-guard.macaroon \
	uri:/lnrpc.Lightning/RegisterRPCMiddleware uri:/lnrpc.Lightning/WalletBalance \
	uri:/lnrpc.Lightning/LookupInvoice >/dev/null
docker compose exec -T lnd-swap chmod 644 /root/.lnd/kiloswap.macaroon /root/.lnd/kiloswap-wide.macaroon /root/.lnd/kiloswap-guard.macaroon
ok "kiloswap (caveat kiloswap=v1), kiloswap-wide, kiloswap-guard"

log "read-only database role"
psql <<'SQL'
DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kiloswap_guard') THEN
    CREATE ROLE kiloswap_guard LOGIN PASSWORD 'guardlab';
  END IF;
END $$;
GRANT CONNECT ON DATABASE boltz TO kiloswap_guard;
GRANT USAGE ON SCHEMA public TO kiloswap_guard;
GRANT SELECT ON "reverseSwaps", swaps TO kiloswap_guard;
SQL
if psql -c 'SET ROLE kiloswap_guard; DELETE FROM swaps WHERE false' >/dev/null 2>&1; then bad "the guard's role can write"; else ok "kiloswap_guard: SELECT on reverseSwaps and swaps only"; fi

echo '{"orders":{},"channels":{}}' >"$work/lsp-state.json"
mkdir -p "$work/state"
start_guard() {
	docker rm -f $GUARD >/dev/null 2>&1 || true
	docker run -d --name $GUARD --network "$NET" \
		-v "$work/kiloswap-guard:/kiloswap-guard:ro" \
		-v "$LAB/data/lnd-swap/tls.cert:/lnd/tls.cert:ro" \
		-v "$LAB/data/lnd-swap/kiloswap-guard.macaroon:/lnd/kiloswap-guard.macaroon:ro" \
		-v "$work/lsp-state.json:/lsp/state.json:ro" -v "$work/state:/state" \
		-e LND_GRPC=lnd-swap:10009 -e LND_TLSCERT=/lnd/tls.cert -e LND_MACAROON=/lnd/kiloswap-guard.macaroon \
		-e DATABASE_URL=postgres://kiloswap_guard:guardlab@postgres:5432/boltz \
		-e LSP_STATE=/lsp/state.json -e LEDGER=/state/ledger.json \
		-e CHAIN_RPC_URL=http://shim:8332/ -e CHAIN_RPC_USER=lab -e CHAIN_RPC_PASSWORD=lab "$@" \
		alpine:3.22 /kiloswap-guard >/dev/null
	for _ in $(seq 1 30); do docker logs $GUARD 2>&1 | grep -q registered && return 0; sleep 1; done
	docker logs $GUARD >&2; return 1
}

log "guard down: the backend's macaroon does nothing"
docker rm -f $GUARD >/dev/null 2>&1 || true
refused "GetInfo without the guard" KS getinfo

log "guard up"
# The lab carries big swaps of earlier runs that pay now: the daily limit
# (unit-tested) is raised for the e2e part and back to default after it
start_guard -e DAILY_SAT=10000000000
ok "registered with lnd-swap ($(docker logs $GUARD 2>&1 | grep -c registered)x)"
allowed "GetInfo through the guard" KS getinfo

log "backend on the guarded macaroon: the e2e scenarios"
cp boltz.conf "$work/boltz.conf.orig"
sed -i 's#macaroonpath = "/lnd/boltz.macaroon"#macaroonpath = "/lnd/kiloswap.macaroon"#' boltz.conf
grep -q kiloswap.macaroon boltz.conf || { bad "boltz.conf not switched"; exit 1; }
docker compose restart boltz >/dev/null
for _ in $(seq 1 60); do r=$(curl -s http://127.0.0.1:29001/v2/swap/submarine || true); [ -n "$r" ] && [ "$r" != "{}" ] && break; sleep 3; done
[ "$r" != "{}" ] || { docker compose logs --tail 30 boltz >&2; bad "backend not up on the guarded macaroon"; exit 1; }
ok "backend up on kiloswap.macaroon"
if (cd "$LAB/src/swap/e2e" && API=http://127.0.0.1:29001 RETENTION_STATE="$work/retention.json" \
	node run.mjs ${E2E:-} big-reverse big-submarine refund-server-down claim-server-down 2>&1 | tee "$work/e2e.log") &&
	[ "$(grep -c '^PASS' "$work/e2e.log")" -ge 4 ]; then
	ok "e2e: $(grep -c '^PASS' "$work/e2e.log") scenarios passed with the guard on"
else
	bad "e2e with the guard: $(grep -c '^FAIL' "$work/e2e.log") failed"; grep '^FAIL' "$work/e2e.log" | cut -c1-300 >&2 || true
fi
# the default scenario list again (run.mjs with names runs only those)
if (cd "$LAB/src/swap/e2e" && API=http://127.0.0.1:29001 RETENTION_STATE="$work/retention.json" \
	node run.mjs 2>&1 | tee "$work/e2e-all.log"); then
	ok "e2e default list: $(grep -c '^PASS' "$work/e2e-all.log") passed"
else
	bad "e2e default list: $(grep -c '^FAIL' "$work/e2e-all.log") failed"; grep '^FAIL' "$work/e2e-all.log" | cut -c1-300 >&2 || true
fi
docker logs $GUARD 2>&1 | grep -E 'allowed|refused' | sed 's/^/    guard: /' | tail -20

start_guard
log "a backend taken over: direct calls with its macaroon"
addr=$(LU newaddress p2wkh | jq -r .address)
refused "SendCoins to a random address" KS sendcoins --addr "$addr" --amt 100000 --force
refused "SendCoins send_all (sweep)" KS sendcoins --addr "$addr" --sweepall --force
refused "SendCoins 2 BTC" KS sendcoins --addr "$addr" --amt 200000000 --force
inv=$(LU addinvoice --amt 50000 | jq -r .payment_request)
refused "pay a random invoice" KS payinvoice --pay_req "$inv" --force --fee_limit 100
pre=$(openssl rand -hex 32); hash=$(printf %s "$pre" | xxd -r -p | sha256sum | cut -d' ' -f1)
LS addholdinvoice "$hash" --amt 20000 >/dev/null
refused "cancel another service's hold invoice" KS cancelinvoice "$hash"
refused "settle another service's hold invoice" KS settleinvoice "$pre"
LS cancelinvoice "$hash" >/dev/null
big=$(openssl rand -hex 32)
refused "a hold invoice over 1 BTC" KS addholdinvoice "$(printf %s "$big" | xxd -r -p | sha256sum | cut -d' ' -f1)" --amt 100000001
upub=$(LU getinfo | jq -r .identity_pubkey)
refused "OpenChannel (not in the macaroon)" KS openchannel --node_key "$upub" --local_amt 1000000
refused "OpenChannel with a too-wide macaroon" WIDE openchannel --node_key "$upub" --local_amt 1000000
refused "SendMany with a too-wide macaroon" WIDE sendmany "{\"$addr\":100000}"
refused "BakeMacaroon with a too-wide macaroon" WIDE bakemacaroon uri:/lnrpc.Lightning/GetInfo
# The backend writes its own database: a row it forged must not be enough
fakepre=$(openssl rand -hex 32); fakehash=$(printf %s "$fakepre" | xxd -r -p | sha256sum | cut -d' ' -f1)
psql -c "INSERT INTO \"reverseSwaps\" (id, version, pair, \"orderSide\", status, \"timeoutBlockHeight\", \"nodeId\", invoice, \"preimageHash\", \"invoiceAmount\", \"onchainAmount\", \"lockupAddress\", fee, \"createdAt\", \"updatedAt\") SELECT 'forged1', version, pair, \"orderSide\", 'invoice.pending', \"timeoutBlockHeight\", \"nodeId\", 'lnbcrtforged', '$fakehash', 150000, 150000, '$addr', 0, now(), now() FROM \"reverseSwaps\" LIMIT 1" >/dev/null
refused "SendCoins to a forged reverse swap (no paid hold invoice in lnd)" KS sendcoins --addr "$addr" --amt 150000 --force
inv2=$(LU addinvoice --amt 70000 | jq -r .payment_request)
psql -c "INSERT INTO swaps (id, version, pair, \"orderSide\", status, \"preimageHash\", invoice, \"invoiceAmount\", \"expectedAmount\", \"onchainAmount\", \"lockupAddress\", \"lockupTransactionId\", \"timeoutBlockHeight\", \"createdRefundSignature\", \"createdAt\", \"updatedAt\") SELECT 'forged2', version, pair, \"orderSide\", 'transaction.confirmed', '$fakehash', '$inv2', 70000, 70000, 70000, '$addr', '$(openssl rand -hex 32)', \"timeoutBlockHeight\", false, now(), now() FROM swaps LIMIT 1" >/dev/null
refused "pay the invoice of a forged submarine swap (no lockup on chain)" KS payinvoice --pay_req "$inv2" --force --fee_limit 121
psql -c "DELETE FROM \"reverseSwaps\" WHERE id = 'forged1'; DELETE FROM swaps WHERE id = 'forged2'" >/dev/null
own=$(openssl rand -hex 32); ownhash=$(printf %s "$own" | xxd -r -p | sha256sum | cut -d' ' -f1)
allowed "a hold invoice of its own (within the limits)" KS addholdinvoice "$ownhash" --amt 20000
allowed "cancel its own hold invoice" KS cancelinvoice "$ownhash"

log "the LSP's reserve"
conf=$(LS walletbalance | jq -r .confirmed_balance)
python3 - "$work/lsp-state.json" "$conf" <<'PY'
import json, sys
path, conf = sys.argv[1], int(sys.argv[2])
# promise the LSP all but 10,000 sat above the 50M reserve
json.dump({"orders": {"lab": {"sats": conf - 50_000_000 - 10_000, "status": "paid"}}, "channels": {}}, open(path, "w"))
PY
h2=$(openssl rand -hex 32 | xxd -r -p | sha256sum | cut -d' ' -f1)
refused "a hold invoice the coins left cannot back" KS addholdinvoice "$h2" --amt 20000
echo '{"orders":{},"channels":{}}' >"$work/lsp-state.json"
echo 'not json' >"$work/lsp-state.json"
refused "anything on-chain with the LSP state unreadable" KS addholdinvoice "$h2" --amt 20000
echo '{"orders":{},"channels":{}}' >"$work/lsp-state.json"

log "pending reverse swaps capped"
start_guard -e MAX_PENDING_REVERSE=0
refused "a hold invoice past the pending limit" KS addholdinvoice "$h2" --amt 20000
start_guard

log "guard stopped again"
docker stop $GUARD >/dev/null
refused "GetInfo with the guard stopped" KS getinfo
refused "SendCoins with the guard stopped" KS sendcoins --addr "$addr" --amt 100000 --force
start_guard
ok "guard back"

printf '\n%d ok, %d failed\n' "$pass" "$failed"
[ "$failed" -eq 0 ]
