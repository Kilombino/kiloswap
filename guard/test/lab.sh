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
( cd "$here/test/hintinv" && CGO_ENABLED=0 go build -trimpath -o "$work/hintinv" . )
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
		-e CHAIN_RPC_URL=http://shim:8332/ -e CHAIN_RPC_USER=lab -e CHAIN_RPC_PASSWORD=lab \
		-e LAUNCH_MODE=0 -e ROUTING_CAP=0 -e MIN_FEE_SAT=121 -e CONF_TIERS=1000000:1,10000000:2,100000000:3 -e REQUIRED_CONFS=1 "$@" \
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
sed -e 's/LND_ADDR/lnd-swap/g' -e 's/LND_TLS_NAME/lnd-swap/g' "$here/../deploy/lnd-proxy/nginx.conf" >"$work/lnd-proxy.conf"
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
    read_only: true
    tmpfs: [/tmp, /var/cache/nginx, /var/run]
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
docker logs $GUARD 2>&1 | grep -q "per op 100000000 sat, daily 200000000 sat.*2 unresolved lockups.*routing cap false" && ok "with the production settings the guard starts with 1 BTC / 2 BTC and routing like upstream (and says so)" || bad "the guard's default limits: $(docker logs $GUARD 2>&1 | grep 'per op' | tail -1)"
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
allowed "a recent page of the wallet history" KS listchaintxns --start_height $((tip - 100)) --end_height -1 --max_transactions 7
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

log "the seventh review: canonical SendCoins, pages of 50, the shim's strict parameters and caps"
refused "SendCoins with a 200-character label (lnd would refuse it after approval)" KS sendcoins --addr "$addr" --amt 100000 --sat_per_vbyte 1 --label "$(printf 'x%.0s' $(seq 200))" --force
refused "SendCoins with min_confs 1000" KS sendcoins --addr "$addr" --amt 100000 --sat_per_vbyte 1 --min_confs 1000 --force
refused "a wallet history page of 8 (raw hex could pass 8 MiB)" KS listchaintxns --start_height $((tip - 100)) --end_height -1 --max_transactions 8
fh=$(openssl rand -hex 32)
LS addinvoice --amt 1000 --preimage "$fh" --memo bridge >/dev/null 2>&1 || true
fhash=$(printf '%s' "$fh" | xxd -r -p | sha256sum | cut -d' ' -f1)
refused "cancelling the operator's own invoice (not a guard hold)" KS cancelinvoice "$fhash"
bh=$(K getbestblockhash)
[ "$(shimcode / '{"id":1,"method":"getblockcount","method":"stop","params":[]}')" = 400 ] && ok "the shim refuses a request with duplicate keys" || bad "the shim took duplicate keys"
[ "$(shimcode / '{"id":1,"method":"getblock","params":{"blockhash":"'"$bh"'"}}')" = 400 ] && ok "the shim refuses named parameters" || bad "the shim took named parameters"
[ "$(shimcode / '{"id":1,"method":"getblock","params":["'"$bh"'",2]}')" = 400 ] && ok "the shim refuses getblock verbosity 2" || bad "the shim took verbosity 2"
[ "$(shimcode / '{"id":1,"method":"getblock","params":["'"$bh"'",1]}')" = 200 ] && ok "the shim answers getblock verbosity 1" || bad "the shim refused getblock verbosity 1"
b10="[$(for i in $(seq 10); do printf '{"method":"getblock","params":["%s",0]},' "$bh"; done | sed 's/,$//')]"
b11="[$(for i in $(seq 11); do printf '{"method":"getblock","params":["%s",0]},' "$bh"; done | sed 's/,$//')]"
b3="[$(for i in $(seq 3); do printf '{"method":"getblock","params":["%s",0]},' "$bh"; done | sed 's/,$//')]"
[ "$(shimcode / "$b3")" = 200 ] && ok "the shim answers a batch of 3 raw blocks (the sidecar's rescan)" || bad "the shim refused 3 raw blocks"
[ "$(shimcode / "$b11")" = 400 ] && ok "the shim refuses 11 whole blocks in one batch" || bad "the shim took 11 blocks"
[ "$(shimcode / "$b10")" = 400 ] && ok "the shim refuses 10 whole blocks in one batch (at most 4)" || bad "the shim took 10 blocks"
knots=$(docker compose ps -q knots)
rss() { docker stats --no-stream --format '{{.MemUsage}}' "$knots" | cut -d/ -f1 | tr -d ' '; }
heavy="[$(for i in $(seq 100); do printf '{"method":"getblock","params":["%s",2]},' "$bh"; done | sed 's/,$//')]"
r0=$(rss)
codes=""
for i in $(seq 20); do codes+="$(shimcode / "$heavy") "; done
r1=$(rss)
[ -z "$(tr -d '4 0' <<<"$codes")" ] && ok "20 batches of 100x getblock verbosity 2 refused before the node (knots RSS $r0 -> $r1)" || bad "heavy batches reached the node: $codes (RSS $r0 -> $r1)"

log "the eighth review: tiny reverse swaps, lockup coverage, lockups per block, big transactions through the shim, ZMQ"
h8=$(openssl rand -hex 32 | xxd -r -p | sha256sum | cut -d' ' -f1)
refused "a 330 sat hold invoice (under the 10000 sat reverse minimum)" KS addholdinvoice "$h8" --amt 330 --expiry 3600
refused "a 330 sat lockup" KS sendcoins --addr "$addr" --amt 330 --sat_per_vbyte 1 --force
sw8=$(curl -s -X POST http://127.0.0.1:29001/v2/swap/reverse -H 'content-type: application/json' -d '{"from":"BTC","to":"BTC","invoiceAmount":330,"preimageHash":"'"$h8"'","claimPublicKey":"02'"$(openssl rand -hex 32)"'"}')
grep -qi "minimal\|minimum\|less than" <<<"$sw8" && ok "the backend refuses a 330 sat reverse swap: ${sw8:0:100}" || bad "330 sat reverse swap: ${sw8:0:200}"
# a griefing loop: reverse swaps paid and never claimed, every lockup asked
# in the same block; the backend stopped so only the backend's macaroon acts
sent8=0; refused8=0; why8=""
# the e2e part's big swaps fill the launch daily limit: raised for the loop
start_guard -e DAILY_SAT=10000000000
K generatetoaddress 1 "$(K getnewaddress)" >/dev/null # a fresh height for the loop
mk8() {
	pre=$(openssl rand -hex 32); ph=$(printf %s "$pre" | xxd -r -p | sha256sum | cut -d' ' -f1)
	cpk=$(openssl ecparam -name secp256k1 -genkey 2>/dev/null | openssl ec -pubout -conv_form compressed -outform DER 2>/dev/null | tail -c 33 | xxd -p -c 99)
	docker compose -f docker-compose.yml -f "$work/override.yml" up -d boltz >/dev/null 2>&1
	for _ in $(seq 1 60); do curl -sf http://127.0.0.1:29001/v2/swap/reverse >/dev/null && break; sleep 2; done
	sw=$(curl -s -X POST http://127.0.0.1:29001/v2/swap/reverse -H 'content-type: application/json' \
		-d "{\"from\":\"BTC\",\"to\":\"BTC\",\"invoiceAmount\":20000,\"preimageHash\":\"$ph\",\"claimPublicKey\":\"$cpk\"}")
	docker compose stop boltz >/dev/null 2>&1
	inv=$(jq -r .invoice <<<"$sw"); la=$(jq -r .lockupAddress <<<"$sw"); oa=$(jq -r .onchainAmount <<<"$sw")
	( timeout 120 docker compose exec -T lnd-user lncli --network=regtest payinvoice --force "$inv" >/dev/null 2>&1 & )
	for _ in $(seq 1 30); do [ "$(LS lookupinvoice "$ph" | jq -r .state)" = ACCEPTED ] && break; sleep 1; done
}
mk8; la1=$la; oa1=$oa
refused "a lockup at 2 sat/vB when the swap was charged at 1 (hold does not cover the lockup and a refund)" KS sendcoins --addr "$la1" --amt "$oa1" --sat_per_vbyte 2 --force
for i in 1 2 3; do
	[ "$i" = 1 ] || mk8
	[ "$i" = 1 ] && { la=$la1; oa=$oa1; }
	if out=$(KS sendcoins --addr "$la" --amt "$oa" --sat_per_vbyte 1 --force 2>&1); then sent8=$((sent8 + 1)); else
		grep -q "a block" <<<"$out" && refused8=$((refused8 + 1)); why8+="[$i: $(grep -o -E 'kiloswap guard: [^"]*' <<<"$out" | head -1 | cut -c1-140)${out:0:80}] "; fi
done
[ "$sent8" = 2 ] && [ "$refused8" = 1 ] && ok "a loop of 3 never-claimed reverse swaps in one block: 2 lockups, the third refused (at most 2 a block)" || bad "lockups per block: sent $sent8, refused $refused8: $why8"
start_guard
K generatetoaddress 1 "$(K getnewaddress)" >/dev/null
docker compose -f docker-compose.yml -f "$work/override.yml" up -d boltz >/dev/null 2>&1
for _ in $(seq 1 60); do curl -sf http://127.0.0.1:29001/v2/swap/reverse >/dev/null && break; sleep 2; done
# a big transaction (2000 outputs) and batches of it through the shim
outs=$(docker compose exec -T knots sh -c 'for i in $(seq 2000); do printf "\"%s\":0.0001," "$(bitcoin-cli -regtest -rpcuser=lab -rpcpassword=lab getnewaddress)"; done' | sed 's/,$//')
bigtx=$(K -named sendmany amounts="{$outs}" fee_rate=1 2>/dev/null || true)
K generatetoaddress 1 "$(K getnewaddress)" >/dev/null
if [ -n "$bigtx" ]; then
	size=$(K gettransaction "$bigtx" | jq -r .hex | wc -c)
	t10="[$(for i in $(seq 10); do printf '{"method":"getrawtransaction","params":["%s"]},' "$bigtx"; done | sed 's/,$//')]"
	t11="[$(for i in $(seq 11); do printf '{"method":"getrawtransaction","params":["%s"]},' "$bigtx"; done | sed 's/,$//')]"
	tv="[$(for i in $(seq 100); do printf '{"method":"getrawtransaction","params":["%s",1]},' "$bigtx"; done | sed 's/,$//')]"
	[ "$(shimcode / "$t10")" = 200 ] && ok "the shim answers 10 raw copies of a $((size / 2)) byte transaction" || bad "10 raw txs refused"
	[ "$(shimcode / "$t11")" = 400 ] && ok "the shim refuses 11 raw transactions in one batch" || bad "11 raw txs taken"
	[ "$(shimcode / '{"id":1,"method":"getrawtransaction","params":["'"$bigtx"'",1]}')" = 200 ] && ok "the shim answers one verbose copy of it" || bad "one verbose big tx refused"
	r0=$(rss); codes=""
	for i in $(seq 10); do codes+="$(shimcode / "$tv") "; done
	r1=$(rss)
	[ -z "$(tr -d '4 0' <<<"$codes")" ] && ok "10 batches of 100 verbose copies refused by the shim (knots RSS $r0 -> $r1)" || bad "verbose batches: $codes"
	bb=$(K gettransaction "$bigtx" | jq -r .blockhash)
	tvk="[$(for i in $(seq 100); do printf '{"method":"getrawtransaction","params":["%s",1,"%s"]},' "$bigtx" "$bb"; done | sed 's/,$//')]"
	printf '%s' "$tvk" > "$work/tv.json"
	r2=$(rss)
	for i in $(seq 10); do docker run --rm -i --network "$NET" curlimages/curl:8.10.1 -s -o /dev/null -u lab:lab -H 'content-type: application/json' --data-binary @- http://knots:18443/ <"$work/tv.json"; done
	r3=$(rss)
	ok "for comparison, the same 10 batches straight to knots: RSS $r2 -> $r3 (each answer ~$(docker run --rm -i --network "$NET" curlimages/curl:8.10.1 -s -o /dev/null -w '%{size_download}' -u lab:lab -H 'content-type: application/json' --data-binary @- http://knots:18443/ <"$work/tv.json") bytes)"
	shimrss=$(docker stats --no-stream --format '{{.MemUsage}}' "$(docker compose ps -q shim)" | cut -d/ -f1)
	ok "shim memory after all of it: $shimrss (limit 256 MB)"
else
	bad "could not make the big transaction"
fi
hwm=$(docker run --rm --network "$NET" curlimages/curl:8.10.1 -s -u lab:lab -H 'content-type: application/json' -d '{"id":1,"method":"getzmqnotifications","params":[]}' http://shim:8332/ | jq -c '[.result[].hwm]' || true)
[ "$hwm" = "[20,20]" ] && ok "knots queues at most 20 ZMQ messages per socket ($hwm)" || bad "zmq hwm: $hwm"

log "the ninth review: getrawmempool alone, AddInvoice without AMP or private channels"
[ "$(shimcode / '[{"id":1,"method":"getrawmempool","params":[]}]')" = 400 ] && ok "the shim refuses getrawmempool inside a batch" || bad "getrawmempool batched"
[ "$(shimcode / '{"id":1,"method":"getrawmempool","params":[]}')" = 200 ] && ok "the shim answers getrawmempool on its own" || bad "getrawmempool refused"
refused "an AMP invoice" KS addinvoice --amt 1000 --amp
refused "a private invoice (advertising private channels)" KS addinvoice --amt 1000 --private
allowed "a plain invoice" KS addinvoice --amt 1000

log "routing like upstream: a hard ceiling of max(121 sat, 0.01%), rewritten by the guard"
# an attacker's invoice: the payment must go through their node (a route
# hint through lnd-user, a 200 sat fee, over the 121 sat ceiling) to a key of theirs
upk=$(LU getinfo | jq -r .identity_pubkey)
start_guard -e DAILY_SAT=10000000000 # the e2e part filled the launch daily limit
subswap() { # invoice -> creates the swap and pays its lockup; the backend pays once it is confirmed
	local cpk sw
	cpk=$(openssl ecparam -name secp256k1 -genkey 2>/dev/null | openssl ec -pubout -conv_form compressed -outform DER 2>/dev/null | tail -c 33 | xxd -p -c 99)
	sw=$(curl -s -X POST http://127.0.0.1:29001/v2/swap/submarine -H 'content-type: application/json' \
		-d "{\"from\":\"BTC\",\"to\":\"BTC\",\"invoice\":\"$1\",\"refundPublicKey\":\"$cpk\"}")
	sid=$(jq -r .id <<<"$sw"); sad=$(jq -r .address <<<"$sw"); samt=$(jq -r .expectedAmount <<<"$sw")
	[ "$sid" != null ] || { echo "no swap: ${sw:0:200}" >&2; return 1; }
	K sendtoaddress "$sad" "$(awk "BEGIN{printf \"%.8f\", $samt/1e8}")" >/dev/null
	sleep 3
	K generatetoaddress 3 "$(K getnewaddress)" >/dev/null
	for _ in $(seq 1 30); do
		curl -s "http://127.0.0.1:29001/v2/swap/$sid" | grep -q -E 'invoice.pending|invoice.failedToPay|transaction.claim' && break; sleep 2
	done
	sleep 10
}
attempts() { docker compose logs lnd-swap 2>&1 | grep -F "for payment $1 failed" | grep -c "Attempt" || true; }
paystate() { LS listpayments --include_incomplete | jq -c --arg h "$1" '[.payments[] | select(.payment_hash == $h)] | last | {status, failure_reason, htlcs: (.htlcs | length)}'; }
upb() { docker compose -f docker-compose.yml -f "$work/override.yml" up -d boltz >/dev/null 2>&1; for _ in $(seq 1 60); do curl -sf http://127.0.0.1:29001/v2/swap/reverse >/dev/null && break; sleep 2; done; }
read -r inv1 ph1 < <("$work/hintinv" 20000 "$upk" 200000 0 | paste -sd' ')
if subswap "$inv1"; then
	stb=$(paystate "$ph1")
	grep -q '"status":"FAILED"' <<<"$stb" && grep -q '"htlcs":0' <<<"$stb" && ok "the backend's own payment through a 200 sat hop: over the 121 sat ceiling, no route, nothing sent ($stb)" || bad "backend payment through the attacker's hop: $stb"
	# the backend taken over asks a 5000 sat limit: the guard lowers it
	docker compose stop boltz >/dev/null 2>&1
	KS payinvoice --pay_req "$inv1" --force --fee_limit 5000 --cltv_limit 40 >"$work/r1" 2>&1 || true
	st1=$(paystate "$ph1")
	grep -q '"status":"FAILED"' <<<"$st1" && grep -q '"htlcs":0' <<<"$st1" && grep -q NO_ROUTE <<<"$st1" \
		&& [ "$(attempts "$ph1")" = 0 ] \
		&& ok "a 5000 sat fee limit through the attacker's 200 sat hop: lowered by the guard to the 121 sat ceiling, no route, never attempted ($st1)" \
		|| bad "attacker route: $st1 $(head -c 200 "$work/r1")"
	upb
	# the backend retries every 15 s; the third "no route" gives the swap up
	for _ in $(seq 1 24); do
		sst=$(curl -s "http://127.0.0.1:29001/v2/swap/$sid"); grep -q "invoice.failedToPay" <<<"$sst" && break
		K generatetoaddress 1 "$(K getnewaddress)" >/dev/null; sleep 5
	done
	grep -q "invoice.failedToPay" <<<"$sst" && grep -q "routing fee limit" <<<"$sst" \
		&& ok "the backend gives the swap up cleanly: $(jq -c . <<<"$sst" | cut -c1-200)" || bad "backend after no route: ${sst:0:240}"
	docker logs $GUARD >"$work/glog" 2>&1
	grep -q 'payment_failed' "$work/glog" && ok "the guard released the payment on lnd's FAILED" || bad "no payment_failed in the guard's log"
else bad "attacker swap not created"; upb; fi
# a 60 sat hop (the same 9-block hint) is under the 121 sat ceiling: it is
# tried (its fake end fails it), so the 200 sat hop failed on its fee only
# and routing is not clamped to the lockup's surplus any more
read -r inv2 ph2 < <("$work/hintinv" 20000 "$upk" 60000 0 | paste -sd' ')
if subswap "$inv2"; then
	n2=$(attempts "$ph2")
	[ "$n2" -gt 0 ] && ok "a 60 sat hop is attempted under the 121 sat ceiling ($n2 attempts in lnd's log; its fake end fails them, as expected)" || {
		bad "the cheap route was not attempted: $(paystate "$ph2")"
		docker compose logs boltz 2>&1 | grep -E "$sid" | grep -iE "pay|cltv|fail" | tail -5 >&2
	}
else bad "cheap swap not created"; fi
start_guard

log "guard stopped again"
docker stop $GUARD >/dev/null
refused "GetInfo with the guard stopped" KS getinfo
refused "SendCoins with the guard stopped" KS sendcoins --addr "$addr" --amt 100000 --force
start_guard
ok "guard back"

printf '\n%d ok, %d failed\n' "$pass" "$failed"
[ "$failed" -eq 0 ]
