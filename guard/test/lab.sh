#!/usr/bin/env bash
# The guard end to end on the regtest lab (paulscode/lightning-fork-swap's
# deploy/regtest with the Kiloswap backend), with the real guard in
# lnd-swap's middleware chain:
#   1. the swaps' server key moved to the guard: the backend runs on the
#      extended public key (image kiloswap-lab/boltz:guard) with a fresh,
#      unrelated seed.dat, on the guarded macaroon; every e2e scenario
#      (both directions, ~1 BTC swaps, user refunds and claims with the
#      server down, the guard's own timeout refund) must still pass, with
#      claims and refunds made by the guard and no cooperative signing;
#   2. a backend taken over: direct lnd calls with the backend's macaroon
#      (and with a too-wide one) that the guard must refuse, including the
#      independent review's scenarios;
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
	[ -f "$work/override.yml" ] && docker compose -f docker-compose.yml -f "$work/override.yml" rm -sf lnd-proxy >/dev/null 2>&1 || true
	sudo -n rm -f "$LAB/data/lnd-swap/kiloswap-proxy.crt"
	if [ -f "$work/seed.orig" ]; then
		sudo -n cp "$work/seed.orig" "$LAB/data/boltz/seed.dat"
	fi
	if [ -f "$work/boltz.conf.orig" ]; then
		cp "$work/boltz.conf.orig" boltz.conf
		docker compose up -d boltz >/dev/null 2>&1 || true
	fi
	rm -rf "$work"
}
trap '[ -n "${KEEP:-}" ] || restore' EXIT

log "guard binary"
( cd "$here" && CGO_ENABLED=0 go build -trimpath -o "$work/kiloswap-guard" ./cmd/kiloswap-guard )
( cd "$here" && CGO_ENABLED=0 go build -trimpath -o "$work/bigreq" ./test/bigreq )
( cd "$here" && CGO_ENABLED=0 go build -trimpath -o "$work/slowloris" ./test/slowloris )
ok "built"

log "macaroons"
BOLTZ_URIS=(
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
	uri:/lnrpc.Lightning/LookupInvoice uri:/lnrpc.Lightning/DecodePayReq \
	uri:/invoicesrpc.Invoices/SettleInvoice uri:/invoicesrpc.Invoices/CancelInvoice \
	uri:/routerrpc.Router/TrackPaymentV2 uri:/lnrpc.Lightning/NewAddress uri:/lnrpc.Lightning/GetTransactions >/dev/null
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
mkdir -p "$work/state" "$work/seed"

log "the swaps' server key moves to the guard"
sudo -n cp "$LAB/data/boltz/seed.dat" "$work/seed.orig"
sudo -n cp "$LAB/data/boltz/seed.dat" "$work/seed/swap-seed"
sudo -n chown "$(id -u)" "$work/seed/swap-seed" "$work/seed.orig"
XPUB=$(SWAP_MNEMONIC_FILE="$work/seed/swap-seed" "$work/kiloswap-guard" xpub)
[[ $XPUB == xpub* ]] && ok "extended public key at m/0/0 for the backend" || { bad "no xpub"; exit 1; }
fresh=$(docker run --rm --entrypoint node -w /boltz-backend kiloswap-lab/boltz:guard -e \
	"const b=require('@scure/bip39');const w=require('@scure/bip39/wordlists/english.js');console.log(b.generateMnemonic(w.wordlist))")
[ "$(wc -w <<<"$fresh")" -eq 12 ] || { bad "no fresh mnemonic"; exit 1; }
printf '%s\n' "$fresh" | sudo -n tee "$LAB/data/boltz/seed.dat" >/dev/null
ok "the backend's seed.dat is now an unrelated mnemonic (the old one only in the guard)"

start_guard() {
	docker rm -f $GUARD >/dev/null 2>&1 || true
	docker run -d --name $GUARD --network "$NET" --memory 160m --memory-swap 160m -e GOMEMLIMIT=100MiB \
		-v "$work/kiloswap-guard:/kiloswap-guard:ro" -v "$work/seed:/seed:ro" \
		-v "$LAB/data/lnd-swap/tls.cert:/lnd/tls.cert:ro" \
		-v "$LAB/data/lnd-swap/kiloswap-guard.macaroon:/lnd/kiloswap-guard.macaroon:ro" \
		-v "$work/lsp-state.json:/lsp/state.json:ro" -v "$work/state:/state" \
		-e LND_GRPC=lnd-swap:10009 -e LND_TLSCERT=/lnd/tls.cert -e LND_MACAROON=/lnd/kiloswap-guard.macaroon \
		-e DATABASE_URL=postgres://kiloswap_guard:guardlab@postgres:5432/boltz \
		-e LSP_STATE=/lsp/state.json -e LEDGER=/state/ledger.json -e NETWORK=regtest \
		-e SWAP_MNEMONIC_FILE=/seed/swap-seed -e API_ADDR=$GUARD:8091 -e API_TOKEN=labtoken \
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
start_guard -e DAILY_SAT=10000000000 -e LAUNCH_MODE=0
ok "registered with lnd-swap ($(docker logs $GUARD 2>&1 | grep -c registered)x)"
allowed "GetInfo through the guard" KS getinfo

log "an inflated lnd wallet history (the backend's listings must stay bounded)"
own=$(LS newaddress p2tr | jq -r .address)
for i in $(seq 1 300); do LS sendcoins --addr "$own" --amt 5000 --sat_per_vbyte 1 --force >/dev/null 2>&1 || true; [ $((i % 50)) = 0 ] && K generatetoaddress 1 "$(K getnewaddress)" >/dev/null; done
K generatetoaddress 1 "$(K getnewaddress)" >/dev/null
ok "lnd-swap's wallet history: $(LS listchaintxns | jq '.transactions | length') transactions"

log "backend on the guarded macaroon and the xpub: the e2e scenarios"
cp boltz.conf "$work/boltz.conf.orig"
# lnd-proxy as in production: nginx's gRPC proxy capping requests at 256 KiB
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 30 \
	-subj /CN=lnd-proxy -addext subjectAltName=DNS:lnd-proxy \
	-keyout "$work/proxy.key" -out "$work/proxy.crt" 2>/dev/null
chmod 644 "$work/proxy.key"
sudo -n cp "$work/proxy.crt" "$LAB/data/lnd-swap/kiloswap-proxy.crt"
sed 's/LND_HOST/lnd-swap/g' "$here/../deploy/lnd-proxy/nginx.conf" >"$work/lnd-proxy.conf"
sed -i 's#^  host = "lnd-swap"#  host = "lnd-proxy"#; s#certpath = "/lnd/tls.cert"#certpath = "/lnd/kiloswap-proxy.crt"#' boltz.conf
sed -i 's#macaroonpath = "/lnd/boltz.macaroon"#macaroonpath = "/lnd/kiloswap.macaroon"#' boltz.conf
# hold invoices within the hour, as the guard asks (deploy/boltz.conf.template)
grep -q '^invoiceExpiry' boltz.conf || sed -i '0,/^swapTypes = .*/s//&\ninvoiceExpiry = 3600/' boltz.conf
grep -q kiloswap.macaroon boltz.conf || { bad "boltz.conf not switched"; exit 1; }
cat >"$work/override.yml" <<YML
services:
  lnd-proxy:
    image: nginx:1.27-alpine
    command: ["nginx", "-c", "/etc/lnd-proxy.conf", "-g", "daemon off;"]
    tmpfs: [/tmp]
    volumes:
      - $work/lnd-proxy.conf:/etc/lnd-proxy.conf:ro
      - $work/proxy.crt:/certs/proxy.crt:ro
      - $work/proxy.key:/certs/proxy.key:ro
      - $LAB/data/lnd-swap/tls.cert:/lnd/tls.cert:ro
  boltz:
    image: kiloswap-lab/boltz:guard
    environment:
      KILOSWAP_SWAP_XPUB: "$XPUB"
      KILOSWAP_GUARD_URL: http://$GUARD:8091
      KILOSWAP_GUARD_TOKEN: labtoken
YML
docker compose -f docker-compose.yml -f "$work/override.yml" up -d lnd-proxy boltz >/dev/null
for _ in $(seq 1 60); do r=$(curl -s http://127.0.0.1:29001/v2/swap/submarine || true); [ -n "$r" ] && [ "$r" != "{}" ] && break; sleep 3; done
[ "$r" != "{}" ] || { docker compose logs --tail 30 boltz >&2; bad "backend not up on the guarded macaroon"; exit 1; }
ok "backend up on kiloswap.macaroon"
signs=1
for _ in $(seq 1 20); do [ "$(docker compose logs boltz 2>&1 | grep -c "signing is the guard" || true)" -gt 0 ] && { signs=0; break; }; sleep 2; done
[ $signs -eq 0 ] && ok "backend: swap keys from the extended public key" || bad "backend still signs"
if [ -n "${SKIP_E2E:-}" ]; then
	ok "e2e skipped (SKIP_E2E)"
elif (cd "$LAB/src/swap/e2e" && NO_COOP=1 API=http://127.0.0.1:29001 RETENTION_STATE="$work/retention.json" \
	node run.mjs ${E2E:-} big-reverse big-submarine refund-server-down claim-server-down reverse-timeout-refund 2>&1 | tee "$work/e2e.log") &&
	[ "$(grep -c '^PASS' "$work/e2e.log")" -ge 5 ]; then
	ok "e2e: $(grep -c '^PASS' "$work/e2e.log") scenarios passed with the guard on"
else
	bad "e2e with the guard: $(grep -c '^FAIL' "$work/e2e.log") failed"; grep '^FAIL' "$work/e2e.log" | cut -c1-300 >&2 || true
fi
# the default scenario list again (run.mjs with names runs only those)
if [ -n "${SKIP_E2E:-}" ]; then
	:
elif (cd "$LAB/src/swap/e2e" && NO_COOP=1 API=http://127.0.0.1:29001 RETENTION_STATE="$work/retention.json" \
	node run.mjs 2>&1 | tee "$work/e2e-all.log"); then
	ok "e2e default list: $(grep -c '^PASS' "$work/e2e-all.log") passed"
else
	bad "e2e default list: $(grep -c '^FAIL' "$work/e2e-all.log") failed"; grep '^FAIL' "$work/e2e-all.log" | cut -c1-300 >&2 || true
fi
{ docker logs $GUARD 2>&1 | grep -E 'allowed|refused|claimed|refunded|settled' | sed 's/^/    guard: /' | tail -30; } || true
n_claim=$(docker logs $GUARD 2>&1 | grep -c '^.* claimed ' || true)
n_refund=$(docker logs $GUARD 2>&1 | grep -c '^.* refunded ' || true)
if [ -z "${SKIP_E2E:-}" ]; then
	[ "$n_claim" -gt 0 ] && ok "the guard made $n_claim claims" || bad "the guard made no claim"
	[ "$n_refund" -gt 0 ] && ok "the guard made $n_refund refunds" || bad "the guard made no refund"
fi

start_guard
log "a backend taken over: direct calls with its macaroon"
addr=$(LU newaddress p2wkh | jq -r .address)
refused "SendCoins to a random address" KS sendcoins --addr "$addr" --amt 100000 --force --sat_per_vbyte 2
refused "SendCoins send_all (sweep)" KS sendcoins --addr "$addr" --sweepall --force
refused "SendCoins 2 BTC" KS sendcoins --addr "$addr" --amt 200000000 --force
inv=$(LU addinvoice --amt 50000 | jq -r .payment_request)
refused "pay a random invoice" KS payinvoice --pay_req "$inv" --force --fee_limit 100 --cltv_limit 40
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
refused "SendCoins to a forged reverse swap (no paid hold invoice in lnd)" KS sendcoins --addr "$addr" --amt 150000 --force --sat_per_vbyte 2
inv2=$(LU addinvoice --amt 70000 | jq -r .payment_request)
psql -c "INSERT INTO swaps (id, version, pair, \"orderSide\", status, \"preimageHash\", invoice, \"invoiceAmount\", \"expectedAmount\", \"onchainAmount\", \"lockupAddress\", \"lockupTransactionId\", \"timeoutBlockHeight\", \"createdRefundSignature\", \"createdAt\", \"updatedAt\") SELECT 'forged2', version, pair, \"orderSide\", 'transaction.confirmed', '$fakehash', '$inv2', 70000, 70000, 70000, '$addr', '$(openssl rand -hex 32)', \"timeoutBlockHeight\", false, now(), now() FROM swaps LIMIT 1" >/dev/null
refused "pay the invoice of a forged submarine swap (no lockup on chain)" KS payinvoice --pay_req "$inv2" --force --fee_limit 121 --cltv_limit 40
psql -c "DELETE FROM \"reverseSwaps\" WHERE id = 'forged1'; DELETE FROM swaps WHERE id = 'forged2'" >/dev/null
log "the independent review's scenarios"
# 1. the amount comes from the invoice: a 5 BTC invoice behind a forged row
#    saying 1 sat, against an old confirmed transaction
big5=$(LU addinvoice --amt 500000000 | jq -r .payment_request 2>/dev/null || true)
oldtx=$(K getblock "$(K getblockhash $(( $(K getblockcount) - 5 )))" | jq -r '.tx[0]')
if [ -n "$big5" ]; then
	psql -c "INSERT INTO swaps (id, version, pair, \"orderSide\", status, \"preimageHash\", invoice, \"invoiceAmount\", \"expectedAmount\", \"onchainAmount\", \"lockupAddress\", \"lockupTransactionId\", \"lockupTransactionVout\", \"keyIndex\", \"refundPublicKey\", \"timeoutBlockHeight\", \"createdRefundSignature\", \"createdAt\", \"updatedAt\") SELECT 'forged3', version, pair, \"orderSide\", 'transaction.confirmed', '$(LU decodepayreq "$big5" | jq -r .payment_hash)', '$big5', 1, 1, 1, '$addr', '$oldtx', 0, \"keyIndex\", \"refundPublicKey\", \"timeoutBlockHeight\", false, now(), now() FROM swaps WHERE version=1 LIMIT 1" >/dev/null
	refused "pay a 5 BTC invoice behind a forged 1 sat row" KS payinvoice --pay_req "$big5" --force --fee_limit 121 --cltv_limit 40
	psql -c "DELETE FROM swaps WHERE id='forged3'" >/dev/null
fi
#    and within the per-operation limit: the old transaction's output is not
#    the swap output the guard rebuilds with our claim key
half=$(LU addinvoice --amt 50000000 | jq -r .payment_request)
psql -c "INSERT INTO swaps (id, version, pair, \"orderSide\", status, \"preimageHash\", invoice, \"invoiceAmount\", \"expectedAmount\", \"onchainAmount\", \"lockupAddress\", \"lockupTransactionId\", \"lockupTransactionVout\", \"keyIndex\", \"refundPublicKey\", \"timeoutBlockHeight\", \"createdRefundSignature\", \"createdAt\", \"updatedAt\") SELECT 'forged5', version, pair, \"orderSide\", 'transaction.confirmed', '$(LU decodepayreq "$half" | jq -r .payment_hash)', '$half', 1, 1, 1, '$addr', '$oldtx', 0, \"keyIndex\", \"refundPublicKey\", $(( $(K getblockcount) + 500 )), false, now(), now() FROM swaps WHERE version=1 LIMIT 1" >/dev/null
refused "pay 0.5 BTC against a lockup that is not our swap output" KS payinvoice --pay_req "$half" --force --fee_limit 5000 --cltv_limit 40
psql -c "DELETE FROM swaps WHERE id='forged5'" >/dev/null
# 3. a hold invoice created elsewhere, paid and held, does not unlock a send
fpre=$(openssl rand -hex 32); fhash=$(printf %s "$fpre" | xxd -r -p | sha256sum | cut -d' ' -f1)
finv=$(LS addholdinvoice "$fhash" --amt 150000 | jq -r .payment_request)
( LU payinvoice --pay_req "$finv" --force >/dev/null 2>&1 & )
for _ in $(seq 1 30); do [ "$(LS lookupinvoice "$fhash" | jq -r .state)" = ACCEPTED ] && break; sleep 1; done
psql -c "INSERT INTO \"reverseSwaps\" (id, version, pair, \"orderSide\", status, \"timeoutBlockHeight\", \"nodeId\", invoice, \"preimageHash\", \"invoiceAmount\", \"onchainAmount\", \"lockupAddress\", \"keyIndex\", \"claimPublicKey\", fee, \"createdAt\", \"updatedAt\") SELECT 'forged4', 1, pair, \"orderSide\", 'invoice.pending', $(( $(K getblockcount) + 144 )), \"nodeId\", '$finv', '$fhash', 150000, 140000, '$addr', \"keyIndex\", \"claimPublicKey\", 0, now(), now() FROM \"reverseSwaps\" WHERE version=1 LIMIT 1" >/dev/null
refused "SendCoins backed by a foreign hold invoice paid and held" KS sendcoins --addr "$addr" --amt 140000 --force --sat_per_vbyte 2
psql -c "DELETE FROM \"reverseSwaps\" WHERE id='forged4'" >/dev/null
# 4. that foreign hold invoice cannot be cancelled or settled, nor claimed as
#    its own by adding it again
refused "cancel a foreign held invoice (bridge-like)" KS cancelinvoice "$fhash"
refused "add a hold invoice with an existing hash" KS addholdinvoice "$fhash" --amt 1000 --expiry 3600
refused "...and cancel it afterwards" KS cancelinvoice "$fhash"
LS cancelinvoice "$fhash" >/dev/null 2>&1 || true
# 5. another service's ordinary invoice cannot be looked up (its preimage)
orh=$(LS addinvoice --amt 1000 | jq -r .r_hash)
refused "look up another service's invoice" KS lookupinvoice "$orh"
# send_all / target_conf
refused "SendCoins with a confirmation target instead of a fee rate" KS sendcoins --addr "$addr" --amt 100000 --conf_target 2 --force

own=$(openssl rand -hex 32); ownhash=$(printf %s "$own" | xxd -r -p | sha256sum | cut -d' ' -f1)
allowed "a hold invoice of its own (within the limits)" KS addholdinvoice "$ownhash" --amt 20000 --expiry 3600
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
refused "a hold invoice the coins left cannot back" KS addholdinvoice "$h2" --amt 20000 --expiry 3600
echo '{"orders":{},"channels":{}}' >"$work/lsp-state.json"
echo 'not json' >"$work/lsp-state.json"
refused "anything on-chain with the LSP state unreadable" KS addholdinvoice "$h2" --amt 20000 --expiry 3600
echo '{"orders":{},"channels":{}}' >"$work/lsp-state.json"

log "pending reverse swaps capped"
start_guard -e MAX_PENDING_REVERSE=0
refused "a hold invoice past the pending limit" KS addholdinvoice "$h2" --amt 20000 --expiry 3600
start_guard

log "the second review's scenarios"
# a backend (superuser here, as Paul's lab) locks the swap tables: the guard
# refuses fast (deadline), lnd keeps answering other calls
( psql -c 'BEGIN; LOCK TABLE swaps, "reverseSwaps" IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(25); COMMIT;' >/dev/null 2>&1 & )
sleep 2
t0=$(date +%s%N)
refused "SendCoins while the tables are locked" KS sendcoins --addr "$addr" --amt 100000 --sat_per_vbyte 2 --force
ms=$(( ($(date +%s%N) - t0) / 1000000 ))
[ "$ms" -lt 3000 ] && ok "refused in ${ms} ms (no queueing behind the lock)" || bad "took ${ms} ms"
t0=$(date +%s%N)
allowed "GetInfo meanwhile" KS getinfo
ms=$(( ($(date +%s%N) - t0) / 1000000 ))
[ "$ms" -lt 2000 ] && ok "answered in ${ms} ms while the tables are locked" || bad "GetInfo took ${ms} ms"
docker logs $GUARD 2>&1 | grep -q -E 'guard_stall|canceling statement|lock timeout|statement timeout' \
	&& ok "the slow query was cut and reported" || bad "no stall/timeout in the guard's log"
sleep 25
h3=$(openssl rand -hex 32 | xxd -r -p | sha256sum | cut -d' ' -f1)
refused "a hold invoice asking a cltv_expiry over the cap" KS addholdinvoice "$h3" --amt 20000 --cltv_expiry_delta 1000 --expiry 3600
refused "a lockup paying the miners 20x the estimate" KS sendcoins --addr "$addr" --amt 100000 --sat_per_vbyte 20 --force

log "the third review: a cancel and a lockup of one hold invoice at once"
# real reverse swaps, the backend stopped once each is paid, then the
# backend's macaroon asks to cancel the invoice and to send the lockup at
# the same moment: never both
race_ok=0
for i in 1 2 3; do
	pre=$(openssl rand -hex 32); ph=$(printf %s "$pre" | xxd -r -p | sha256sum | cut -d' ' -f1)
	cpk=$(openssl ecparam -name secp256k1 -genkey 2>/dev/null | openssl ec -pubout -conv_form compressed -outform DER 2>/dev/null | tail -c 33 | xxd -p -c 99)
	sw=$(curl -s -X POST http://127.0.0.1:29001/v2/swap/reverse -H 'content-type: application/json' \
		-d "{\"from\":\"BTC\",\"to\":\"BTC\",\"invoiceAmount\":30000,\"preimageHash\":\"$ph\",\"claimPublicKey\":\"$cpk\"}")
	inv=$(jq -r .invoice <<<"$sw"); la=$(jq -r .lockupAddress <<<"$sw"); oa=$(jq -r .onchainAmount <<<"$sw")
	[ "$inv" != null ] || { bad "race $i: no reverse swap: ${sw:0:200}"; continue; }
	docker compose stop boltz >/dev/null 2>&1
	( timeout 120 docker compose exec -T lnd-user lncli --network=regtest payinvoice --force "$inv" >/dev/null 2>&1 & )
	for _ in $(seq 1 30); do [ "$(LS lookupinvoice "$ph" | jq -r .state)" = ACCEPTED ] && break; sleep 1; done
	( if KS cancelinvoice "$ph" >"$work/c$i" 2>&1; then echo 0; else echo 1; fi >"$work/cx$i" ) &
	( if KS sendcoins --addr "$la" --amt "$oa" --sat_per_vbyte 1 --force >"$work/s$i" 2>&1; then echo 0; else echo 1; fi >"$work/sx$i" ) &
	wait
	c=$(cat "$work/cx$i"); sx=$(cat "$work/sx$i"); st=$(LS lookupinvoice "$ph" | jq -r .state)
	if [ "$c" = 0 ] && [ "$sx" = 0 ]; then bad "race $i: both the cancel and the lockup went through"
	elif [ "$sx" = 0 ] && [ "$st" != ACCEPTED ]; then bad "race $i: lockup sent and the invoice is $st"
	else race_ok=$((race_ok + 1)); fi
	docker compose -f docker-compose.yml -f "$work/override.yml" up -d boltz >/dev/null 2>&1
	for _ in $(seq 1 60); do curl -sf http://127.0.0.1:29001/v2/swap/reverse >/dev/null && break; sleep 2; done
done
[ "$race_ok" = 3 ] && ok "cancel vs lockup: never both, 3 of 3 (cancel/send exits: $(cat "$work"/cx* | tr '\n' ' ')/ $(cat "$work"/sx* | tr '\n' ' '))"

log "the fourth review: a huge request must not end the guard's stream"
ends=$(docker logs $GUARD 2>&1 | grep -c "stream ended" || true)
out=$(docker run --rm --network "$NET" -v "$work/bigreq:/bigreq:ro" \
	-v "$LAB/data/lnd-swap/tls.cert:/lnd/tls.cert:ro" -v "$LAB/data/lnd-swap/kiloswap.macaroon:/lnd/kiloswap.macaroon:ro" \
	alpine:3.22 /bigreq lnd-swap:10009 /lnd/tls.cert /lnd/kiloswap.macaroon 5000000 2>&1 || true)
grep -q "request of" <<<"$out" && ok "a 5 MB DecodePayReq refused by size: ${out:0:120}" || bad "5 MB request: $out"
sleep 2
[ "$(docker logs $GUARD 2>&1 | grep -c "stream ended" || true)" = "$ends" ] && ok "the guard's stream did not end" || bad "the stream ended after the huge request"
allowed "GetInfo right after" KS getinfo

log "the fifth review: huge requests through lnd-proxy, memory, slow clients"
restarts() { docker inspect -f '{{.RestartCount}} {{.State.OOMKilled}} {{.State.Running}}' $GUARD; }
before=$(restarts)
proxy=$(docker compose -f docker-compose.yml -f "$work/override.yml" ps -q lnd-proxy)
pnet=$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$proxy" | awk '{print $1}')
out=$(docker run --rm --network "$pnet" -v "$work/bigreq:/bigreq:ro" -v "$work/proxy.crt:/lnd/tls.cert:ro" \
	-v "$LAB/data/lnd-swap/kiloswap.macaroon:/lnd/kiloswap.macaroon:ro" \
	alpine:3.22 /bigreq lnd-proxy:10009 /lnd/tls.cert /lnd/kiloswap.macaroon 40000000 2>&1 || true)
grep -q -i -E "413|too large|resourceexhausted|ResourceExhausted|RST_STREAM|unexpected HTTP status" <<<"$out" \
	&& ok "a 40 MB request stops at lnd-proxy: ${out:0:140}" || bad "40 MB through the proxy: $out"
out=$(docker run --rm --network "$NET" -v "$work/bigreq:/bigreq:ro" \
	-v "$LAB/data/lnd-swap/tls.cert:/lnd/tls.cert:ro" -v "$LAB/data/lnd-swap/kiloswap.macaroon:/lnd/kiloswap.macaroon:ro" \
	alpine:3.22 /bigreq lnd-swap:10009 /lnd/tls.cert /lnd/kiloswap.macaroon 40000000 2>&1 || true)
sleep 5
after=$(restarts)
[ "$after" = "$before" ] && [[ $after == *" false true" ]] && ok "40 MB straight to lnd (no proxy): the guard in 160 MB was not killed ($after)" || bad "guard restarted or OOM: $before -> $after"
for _ in $(seq 1 30); do docker logs $GUARD 2>&1 | tail -5 | grep -q registered && break; sleep 1; done
allowed "GetInfo after the huge requests" KS getinfo
gip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' $GUARD)
docker run --rm --network "$NET" -v "$work/slowloris:/slowloris:ro" alpine:3.22 sh -c "ulimit -n 10000; /slowloris $gip:8091 4500 15" >"$work/slow.out" 2>&1 &
sleep 8
mid=$(restarts)
wait
sleep 4
code=$(docker run --rm --network "$NET" curlimages/curl:8.10.1 -s -o /dev/null -w '%{http_code}' -X POST "http://$gip:8091/v1/health" 2>/dev/null || true)
[ "$(restarts)" = "$before" ] && [ "$mid" = "$before" ] && ok "4500 slow connections ($(cat "$work/slow.out")): the guard stayed up" || bad "the guard went down under slow connections: $(restarts)"
[ "$code" = 204 ] && ok "the API answers again after them" || bad "API after slowloris: $code"

log "the sixth review: canonical addresses, bounded listings, memo nonce, the shim's limits"
upaddr=$(tr '[:lower:]' '[:upper:]' <<<"$addr")
refused "SendCoins to an uppercase (non-canonical) address" KS sendcoins --addr "$upaddr" --amt 100000 --sat_per_vbyte 1 --force
refused "the whole wallet history (from height 0, unbounded)" KS listchaintxns --start_height 0
tip=$(K getblockcount)
allowed "a recent page of the wallet history" KS listchaintxns --start_height $((tip - 100)) --end_height -1 --max_transactions 100
nh=$(openssl rand -hex 32 | xxd -r -p | sha256sum | cut -d' ' -f1)
allowed "a hold invoice (memo of its own)" KS addholdinvoice "$nh" --amt 20000 --expiry 3600 --memo backend-memo
memo=$(LS lookupinvoice "$nh" | jq -r .memo)
[[ $memo == kiloswap\ * ]] && ok "lnd made it with the guard's memo nonce ($memo)" || bad "the hold's memo is '$memo', not the guard's"
KS cancelinvoice "$nh" >/dev/null 2>&1 || true
shim() { docker compose exec -T knots sh -c "wget -q -O- --header 'content-type: application/json' --post-data '$2' http://lab:lab@shim:8332$1 2>&1 || true"; }
shimcode() { docker run --rm --network "$NET" curlimages/curl:8.10.1 -s -o /dev/null -w '%{http_code}' -u lab:lab -H 'content-type: application/json' -d "$2" "http://shim:8332$1"; }
[ "$(shimcode / '[{"id":1,"method":"getblockcount","params":[]},{"id":2,"method":"stop","params":[]}]')" = 403 ] && ok "the shim refuses a batch with a method outside its list" || bad "the shim took a batch with stop"
[ "$(shimcode / '{"id":1,"method":"stop","params":[]}')" = 403 ] && ok "the shim refuses methods outside its list (stop)" || bad "the shim forwarded stop"
[ "$(shimcode /wallet/other '{"id":1,"method":"listunspent","params":[]}')" = 403 ] && ok "the shim refuses other wallets" || bad "the shim reached another wallet"
[ "$(shimcode / '{"id":1,"method":"getblockcount","params":[]}')" = 200 ] && ok "the shim still answers the backend's methods" || bad "the shim refused getblockcount"

log "guard stopped again"
docker stop $GUARD >/dev/null
refused "GetInfo with the guard stopped" KS getinfo
refused "SendCoins with the guard stopped" KS sendcoins --addr "$addr" --amt 100000 --force
start_guard
ok "guard back"

printf '\n%d ok, %d failed\n' "$pass" "$failed"
[ "$failed" -eq 0 ]
