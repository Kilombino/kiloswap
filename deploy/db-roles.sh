#!/usr/bin/env bash
# Read-only database roles, after the backend's first start created its
# tables: kiloswap_guard (the guard) and kiloswap_watch (the bot) may read
# the two swap tables and nothing else.
set -euo pipefail
cd "$(dirname "$0")"
set -a; . ./.env; set +a
R=${KILOSWAP_ROOT:?}
gpw=$(cat "$R/secrets/guard_db_password")
# roles are made by the superuser, grants by boltz (the tables' owner)
admin() { docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -q -U pgadmin -d boltz "$@"; }
psql() { docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -q -U boltz -d boltz "$@"; }
admin -v gpw="$gpw" <<'SQL'
SELECT format('CREATE ROLE kiloswap_guard LOGIN NOSUPERUSER PASSWORD %L', :'gpw')
  WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kiloswap_guard') \gexec
SELECT 'CREATE ROLE kiloswap_watch LOGIN NOSUPERUSER'
  WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kiloswap_watch') \gexec
-- the guard's statements never wait long, whatever its connection asks
ALTER ROLE kiloswap_guard SET statement_timeout = '1s';
ALTER ROLE kiloswap_guard SET lock_timeout = '500ms';
ALTER ROLE kiloswap_guard SET default_transaction_read_only = on;
SQL
if admin -tAc "SELECT rolsuper FROM pg_roles WHERE rolname = 'boltz'" | grep -q t; then
	echo "the backend's role boltz is a superuser: stop (see postgres-init)" >&2; exit 1
fi
psql <<'SQL'
GRANT CONNECT ON DATABASE boltz TO kiloswap_guard, kiloswap_watch;
GRANT USAGE ON SCHEMA public TO kiloswap_guard, kiloswap_watch;
GRANT SELECT ON "reverseSwaps", swaps TO kiloswap_guard, kiloswap_watch;
SQL
if psql -c 'SET ROLE kiloswap_guard; DELETE FROM swaps WHERE false' >/dev/null 2>&1; then
	echo "kiloswap_guard can write: stop" >&2; exit 1
fi
echo "kiloswap_guard / kiloswap_watch: SELECT on reverseSwaps and swaps only"
