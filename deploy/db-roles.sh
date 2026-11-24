#!/usr/bin/env bash
# Read-only database roles, after the backend's first start created its
# tables: kiloswap_guard (the guard) and kiloswap_watch (the bot) may read
# the two swap tables and nothing else.
set -euo pipefail
cd "$(dirname "$0")"
set -a; . ./.env; set +a
R=${KILOSWAP_ROOT:?}
gpw=$(cat "$R/secrets/guard_db_password")
psql() { docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -q -U boltz -d boltz "$@"; }
psql -v gpw="$gpw" <<'SQL'
SELECT format('CREATE ROLE kiloswap_guard LOGIN PASSWORD %L', :'gpw')
  WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kiloswap_guard') \gexec
SELECT 'CREATE ROLE kiloswap_watch LOGIN'
  WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kiloswap_watch') \gexec
GRANT CONNECT ON DATABASE boltz TO kiloswap_guard, kiloswap_watch;
GRANT USAGE ON SCHEMA public TO kiloswap_guard, kiloswap_watch;
GRANT SELECT ON "reverseSwaps", swaps TO kiloswap_guard, kiloswap_watch;
SQL
if psql -c 'SET ROLE kiloswap_guard; DELETE FROM swaps WHERE false' >/dev/null 2>&1; then
	echo "kiloswap_guard can write: stop" >&2; exit 1
fi
echo "kiloswap_guard / kiloswap_watch: SELECT on reverseSwaps and swaps only"
