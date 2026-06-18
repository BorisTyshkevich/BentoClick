#!/usr/bin/env bash
# bentoclick uninstall — drops the dashboard database, sanitize_panel
# function, the writer/reader roles, and the runtime asset files in
# user_files. Idempotent.

set -euo pipefail

DB="bentoclick"
for arg in "$@"; do
  case "$arg" in
    --ch-host=*)     CH_HOST="${arg#*=}" ;;
    --ch-user=*)     CH_USER="${arg#*=}" ;;
    --ch-password=*) CH_PASSWORD="${arg#*=}" ;;
    --db=*)          DB="${arg#*=}" ;;
    *) echo "ERROR: unknown arg: $arg" >&2; exit 2 ;;
  esac
done

: "${CH_HOST:?--ch-host required}"
: "${CH_USER:?--ch-user required}"
CH_PASSWORD="${CH_PASSWORD:-}"

ch_query() {
  curl -fsS --user "${CH_USER}:${CH_PASSWORD}" \
    --data-binary @- "${CH_HOST}/?multi_statements=1"
}

echo "==> dropping ${DB}.* objects"
cat <<SQL | ch_query
DROP TABLE    IF EXISTS ${DB}.dashboards_mv SYNC;
DROP TABLE    IF EXISTS ${DB}.dashboards    SYNC;
DROP TABLE    IF EXISTS ${DB}.dashboards_raw SYNC;
DROP VIEW     IF EXISTS ${DB}.dashboards_prefix;
DROP VIEW     IF EXISTS ${DB}.whoami;
DROP ROLE     IF EXISTS ${DB}_reader_role;
DROP ROLE     IF EXISTS ${DB}_writer_role;
DROP USER     IF EXISTS ${DB}_definer;
DROP DATABASE IF EXISTS ${DB} SYNC;
DROP FUNCTION IF EXISTS sanitize_json_text;
SQL

echo "==> removing dash/ files from user_files (best-effort)"
for path in spa.html spa.js spa-helpers.js dash.js charts.js dash-theme.css tweaks.js oauth-callback.html mcp-callback.html config.json client.json bentoclick.xml; do
  printf "INSERT INTO FUNCTION file('dash/%s', 'RawBLOB', 'String') SELECT ''" "$path" \
    | ch_query >/dev/null 2>&1 || true
done

echo "==> done."
