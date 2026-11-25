#!/usr/bin/env bash
# The compose file without any limits in the environment must give the
# guard the launch limits (0.1 BTC per operation, 0.25 BTC a day), and so
# must .env.example.
set -euo pipefail
cd "$(dirname "$0")"
cfg=$(env -i PATH="$PATH" HOME="$HOME" KILOSWAP_ROOT=/x LND_HOST=x LND_DIR=/x LSP_STATE=/x NODE_HOST=x NODE_RPC_PASSWORD=x \
	docker compose -f docker-compose.yml --env-file /dev/null config 2>/dev/null)
grep -q 'MAX_PER_OP_SAT: "10000000"' <<<"$cfg" || { echo "compose default MAX_PER_OP_SAT is not 10000000"; exit 1; }
grep -q 'DAILY_SAT: "25000000"' <<<"$cfg" || { echo "compose default DAILY_SAT is not 25000000"; exit 1; }
grep -q 'MAX_UNRESOLVED: "2"' <<<"$cfg" || { echo "compose default MAX_UNRESOLVED is not 2"; exit 1; }
grep -q '^MAX_PER_OP_SAT=10000000$' .env.example || { echo ".env.example MAX_PER_OP_SAT"; exit 1; }
grep -q '^DAILY_SAT=25000000$' .env.example || { echo ".env.example DAILY_SAT"; exit 1; }
grep -q '^maxSwapAmount = 10000000' boltz.conf.template || { echo "boltz.conf maxSwapAmount"; exit 1; }
echo "launch limits by default: ok"
