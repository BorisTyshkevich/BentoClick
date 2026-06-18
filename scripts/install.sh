#!/usr/bin/env bash
# bentoclick installer.
#
# Applies the v1 schema to a ClickHouse instance, pushes the SPA runtime
# (runtime/v1/) and HTTP handlers (handlers/) to user_files via
# INSERT INTO FUNCTION file(), substitutes config templates with
# install-time values, and inserts the four sample dashboards through
# the sanitizing MV.
#
# This script is intentionally generic. ACM-specific deployment glue
# (cluster lifecycle, Auth0 OIDC client, secret rotation) is the
# caller's job — wrap this script with whatever your environment
# needs.
#
# Usage:
#   ./install.sh \
#     --ch-host=https://<host>:<port>   \   # ClickHouse HTTPS root
#     --ch-user=<admin>                 \   # admin user with CREATE/INSERT
#     --ch-password=<pw>                \   # admin password (or empty)
#     --mcp-url=https://<host>          \   # MCP origin (no /mcp suffix) — the SPA appends
#                                          # /.well-known/oauth-protected-resource (RFC 9728)
#     --spa-origin=https://<host>       \   # SPA's public origin
#     [--db=bentoclick]                 \   # dashboard database name
#     [--cluster='{cluster}']           \   # CH cluster name (default: the {cluster} macro)
#     [--migrate-from=<old-db>]         \   # copy rows from old DB after schema apply
#     [--brand-name=bentoclick]         \   # browser-tab title
#     [--accent=#00d4aa]                    # primary accent color

set -euo pipefail

# ---- defaults ----
DB="bentoclick"
# Cluster name for ON CLUSTER + clusterAllReplicas-based asset
# distribution. Default to the literal {cluster} macro which CH
# expands at parse time on any cluster that defines the macro
# (antalya does; the test image's clickhouse-config.xml does).
CLUSTER="{cluster}"
MIGRATE_FROM=""
BRAND_NAME="bentoclick"
ACCENT="#00d4aa"
# Static OAuth client registered with the AS (Auth0). CIMD is reserved for
# the MCP's dynamic clients; this SPA uses a real registered app instead.
OAUTH_CLIENT_ID=""
OAUTH_AUDIENCE=""
OAUTH_ORGANIZATION=""

# ---- arg parse ----
for arg in "$@"; do
  case "$arg" in
    --ch-host=*)      CH_HOST="${arg#*=}" ;;
    --ch-user=*)      CH_USER="${arg#*=}" ;;
    --ch-password=*)  CH_PASSWORD="${arg#*=}" ;;
    --mcp-url=*)      MCP_URL="${arg#*=}" ;;
    --spa-origin=*)   SPA_ORIGIN="${arg#*=}" ;;
    --db=*)           DB="${arg#*=}" ;;
    --cluster=*)      CLUSTER="${arg#*=}" ;;
    --migrate-from=*) MIGRATE_FROM="${arg#*=}" ;;
    --brand-name=*)   BRAND_NAME="${arg#*=}" ;;
    --accent=*)       ACCENT="${arg#*=}" ;;
    --oauth-client-id=*) OAUTH_CLIENT_ID="${arg#*=}" ;;
    --oauth-audience=*)  OAUTH_AUDIENCE="${arg#*=}" ;;
    --oauth-organization=*) OAUTH_ORGANIZATION="${arg#*=}" ;;
    *) echo "ERROR: unknown arg: $arg" >&2; exit 2 ;;
  esac
done

: "${CH_HOST:?--ch-host required}"
: "${CH_USER:?--ch-user required}"
: "${MCP_URL:?--mcp-url required}"
: "${SPA_ORIGIN:?--spa-origin required}"
CH_PASSWORD="${CH_PASSWORD:-}"

# Scheme+authority of the MCP URL (no path / query). Used to fill the
# ${MCP_ORIGIN} placeholder in the handler XML's connect-src CSP so
# the SPA's RFC 9728 discovery fetch (MCP/.well-known/...) isn't
# blocked when MCP and SPA are on different origins.
MCP_ORIGIN="$(printf '%s' "$MCP_URL" | sed -E 's|^(https?://[^/]+).*|\1|')"

# Resolve the repo root (this script lives in scripts/) so the relative
# paths below — schema/, runtime/v1/, handlers/, config/, samples/ —
# resolve regardless of the caller's cwd.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

# ClickHouse auth/query (setup_netrc, ch_curl, ch_query), the cluster
# asset uploader (ch_file_upload), and the JS bundler + canonical bundle
# lists (bundle_concat, DASH_BUNDLE, CHARTS_BUNDLE, VERBATIM_ASSETS,
# push_asset) are shared with update.sh — see scripts/lib.sh.
source "$SCRIPT_DIR/lib.sh"
setup_netrc "$CH_HOST" "$CH_USER" "$CH_PASSWORD"

# Apply a SQL file by splitting it on bare `;` lines and sending each
# statement separately. Sufficient for our schema files; not a full
# SQL parser.
ch_apply_file() {
  local file="$1"
  python3 - <<PY
import re, sys, urllib.request, base64, ssl, os

raw = open("${file}").read()
# Substitute templates.
raw = raw.replace("\${DB}", "${DB}").replace("\${SPA_ORIGIN}", "${SPA_ORIGIN}")

# Strip comment-only lines, then split on lines that END with ';'.
stmts, buf = [], []
for line in raw.splitlines():
    s = line.strip()
    if not s or s.startswith("--"):
        continue
    buf.append(line)
    if s.endswith(";"):
        stmt = "\n".join(buf).rstrip().rstrip(";").strip()
        if stmt:
            stmts.append(stmt)
        buf = []
if buf:
    tail = "\n".join(buf).strip().rstrip(";").strip()
    if tail:
        stmts.append(tail)

with open("${NETRC_FILE}") as f:
    netrc = f.read()
import re as _re
m = _re.search(r"login\s+(\S+).*?password\s+(\S+)", netrc, _re.S)
user, pw = m.group(1), m.group(2)

auth = "Basic " + base64.b64encode(f"{user}:{pw}".encode()).decode()
for i, stmt in enumerate(stmts):
    req = urllib.request.Request("${CH_HOST}/", data=stmt.encode(),
                                  headers={"Authorization": auth})
    try:
        urllib.request.urlopen(req).read()
    except urllib.error.HTTPError as e:
        sys.stderr.write(f"\n[ch_apply_file {os.path.basename('${file}')}] stmt {i+1} failed:\n{stmt[:200]}\n--\n{e.read().decode(errors='replace')[:500]}\n")
        raise SystemExit(1)
PY
}

echo "==> bentoclick install"
echo "    CH:           $CH_HOST"
echo "    DB:           $DB"
echo "    MCP:          $MCP_URL"
echo "    MCP_ORIGIN:   $MCP_ORIGIN   (substituted into handler CSP)"
echo "    SPA:          $SPA_ORIGIN"

# ---- 1. Apply schema ----
echo "==> applying schema/*.sql"
for sql in schema/00-definer.sql schema/01-database.sql schema/02-roles.sql; do
  ch_apply_file "$sql"
done

# ---- 1b. Optional: copy data from a previous DB ----
# Use case: renaming the dashboard database (e.g. `dashboards` →
# `bentoclick`) without losing user content. The MV doesn't fire on
# this direct INSERT into the destination — that's correct, the
# source rows were already sanitized at original write time.
if [[ -n "${MIGRATE_FROM}" ]]; then
  echo "==> migrating data from ${MIGRATE_FROM} → ${DB}"
  printf "INSERT INTO %s.dashboards SELECT * FROM %s.dashboards FINAL" \
    "$DB" "$MIGRATE_FROM" | ch_query
fi

# ---- 2. Push runtime assets ----
#
# dash.js and charts.js source is split across runtime/v1/{core,panels,charts}/
# for human-readability, but the iframe boot fetches a single
# /lib/v1/dash.js and a single /lib/v1/charts.js. So we concatenate
# each tree in topological order at deploy time and upload the
# resulting bundle as the file the handler serves.
#
# The iframe's `moduleToClassic` strips `import` / `export` at boot,
# so the bundled file can keep ES-module syntax — useful for direct
# inspection and for vitest, which loads each module natively.
#
# Order matters within each bundle: non-hoisted declarations
# (`const`, `class`) must appear before any reference. Functions
# are hoisted, so they can be in any order within their file.

echo "==> bundling + pushing runtime/v1/* to user_files"
push_asset charts.js   # bundled from CHARTS_BUNDLE
push_asset dash.js     # bundled from DASH_BUNDLE
for a in "${VERBATIM_ASSETS[@]}"; do
  push_asset "$a"
done

# ---- 3. Push HTTP handlers ----
# Substitute ${MCP_ORIGIN} in each XML before upload so the SPA's
# Content-Security-Policy allowlists the actual MCP host. Without
# this, the literal `${MCP_ORIGIN}` shipped to CH is silently
# ignored by browser CSP parsers — leaving `connect-src 'self'`,
# which only works when MCP and SPA share an origin.
echo "==> pushing handlers/* to user_files (substituting \${MCP_ORIGIN})"
for f in handlers/*; do
  base="$(basename "$f")"
  tmp_handler="$(mktemp)"
  sed -e "s|\${MCP_ORIGIN}|${MCP_ORIGIN}|g" "$f" > "$tmp_handler"
  ch_file_upload "dash/${base}" "$tmp_handler"
  rm -f "$tmp_handler"
done

# ---- 4. Render config.json + client.json ----
echo "==> rendering and pushing config templates"
tmp_config="$(mktemp)"
sed -e "s|\${CH_URL}|${CH_HOST}|g" \
    -e "s|\${MCP_URL}|${MCP_URL}|g" \
    -e "s|\${SPA_ORIGIN}|${SPA_ORIGIN}|g" \
    -e "s|\${DB}|${DB}|g" \
    -e "s|\${BRAND_NAME}|${BRAND_NAME}|g" \
    -e "s|\${ACCENT}|${ACCENT}|g" \
    -e "s|\${OAUTH_CLIENT_ID}|${OAUTH_CLIENT_ID}|g" \
    -e "s|\${OAUTH_AUDIENCE}|${OAUTH_AUDIENCE}|g" \
    -e "s|\${OAUTH_ORGANIZATION}|${OAUTH_ORGANIZATION}|g" \
    config/config.json.tmpl > "$tmp_config"
ch_file_upload "dash/config.json" "$tmp_config"

tmp_client="$(mktemp)"
sed -e "s|\${SPA_ORIGIN}|${SPA_ORIGIN}|g" \
    -e "s|\${BRAND_NAME}|${BRAND_NAME}|g" \
    config/client.json.tmpl > "$tmp_client"
ch_file_upload "dash/client.json" "$tmp_client"

rm -f "$tmp_config" "$tmp_client"

# ---- 5. Insert sample dashboards ----
# Through dashboards_raw so the MV runs sanitization. Each sample lives
# as a JSON file in samples/ and is inserted as a single row with
# title=slug-title, panels=loaded JSON, params=loaded JSON.
echo "==> inserting sample dashboards"
for spec in samples/*.spec.json; do
  slug="$(basename "$spec" .spec.json)"
  title="$(python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('title', sys.argv[2]))" "$spec" "$slug")"
  subtitle="$(python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('subtitle', ''))" "$spec")"
  params_json="$(python3 -c "import json,sys; print(json.dumps(json.load(open(sys.argv[1])).get('params', [])))" "$spec")"
  panels_json="$(python3 -c "import json,sys; print(json.dumps(json.load(open(sys.argv[1])).get('panels', [])))" "$spec")"
  spec_version="$(python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('spec_version', 1))" "$spec")"
  # SELECT-form INSERT (CH 26.3 currentUser()-in-VALUES quirk avoidance).
  # SQL string literals interpret `\n`, `\t`, etc.; we must double every
  # backslash before splicing JSON so the JSON parser sees the original
  # `\n` escape sequence (two chars), not a real newline. Single quotes
  # are doubled by SQL convention. Order matters: backslash first, then
  # single quote, so the second pass doesn't escape the doubling.
  esc() {
    local s="$1"
    s="${s//\\/\\\\}"
    s="${s//\'/\'\'}"
    printf '%s' "$s"
  }
  # dashboards_raw stores params and panels as plain String now (the MV
  # JSONExtract's on the way to `dashboards`). Just splice the JSON text
  # in — no JSONExtract wrap needed at this layer.
  printf "INSERT INTO ${DB}.dashboards_raw (slug, title, subtitle, spec_version, params, panels) SELECT '%s', '%s', '%s', %s, '%s', '%s'" \
    "$(esc "$slug")" \
    "$(esc "$title")" \
    "$(esc "$subtitle")" \
    "$spec_version" \
    "$(esc "$params_json")" \
    "$(esc "$panels_json")" \
    | ch_query
  echo "    + $slug"
done

echo "==> done. Open ${SPA_ORIGIN}/b/app to see your dashboards."
