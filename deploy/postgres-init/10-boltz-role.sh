#!/bin/sh
# First start only (the image runs /docker-entrypoint-initdb.d once, on an
# empty data directory): the backend's role is an ordinary owner of its
# own database, not a superuser. A superuser could lock every table of the
# server or read past any grant; the superuser (pgadmin) stays here for
# maintenance only.
set -eu
pw=$(cat /run/secrets/postgres_password)
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres -v pw="$pw" <<'SQL'
SELECT format('CREATE ROLE boltz LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION CONNECTION LIMIT 25 PASSWORD %L', :'pw') \gexec
CREATE DATABASE boltz OWNER boltz;
SQL
