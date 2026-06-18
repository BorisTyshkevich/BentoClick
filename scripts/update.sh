#!/usr/bin/env bash
# bentoclick asset updater — push one or more SPA asset files (the bundled
# dash.js / charts.js, or any verbatim runtime/v1/* file) to a running
# ClickHouse cluster's user_files, WITHOUT touching schema, roles, HTTP
# handlers, config templates, or sample dashboards.
#
# Use this for the common case of shipping a JS/CSS/HTML change to the SPA
# — re-running install.sh for that is overkill (and would re-apply schema,
# re-substitute the handler CSP, and re-insert samples).
#
# Usage:
#   ./scripts/update.sh \
#     --ch-host=https://<host>:<port>   \   # ClickHouse HTTPS root
#     --ch-user=<admin>                 \   # admin user with CREATE/INSERT
#     [--ch-password=<pw>]              \   # or set CH_PASSWORD in the env (preferred)
#     [--db=bentoclick]                 \   # dashboard database name
#     [--cluster='{cluster}']           \   # CH cluster name
#     [asset ...]                           # e.g. dash.js charts.js spa.js dash-theme.css
#
# With no asset args, pushes the full SPA asset set (dash.js + charts.js +
# all verbatim runtime files). Asset names are the served basenames under
# /lib/v1/ (dash.js, charts.js, spa.html, spa.js, spa-helpers.js, tweaks.js,
# dash-theme.css, oauth-callback.html).
#
# Secret handling: pass the password via the CH_PASSWORD env var rather than
# --ch-password so it never lands on the process argv (visible in `ps`) or in
# shell history. Either way it is written only to a chmod 600 .netrc tempfile.

set -euo pipefail

# ---- defaults ----
DB="bentoclick"
CLUSTER="{cluster}"
# Password defaults to the CH_PASSWORD env var; --ch-password overrides.
CH_PASSWORD="${CH_PASSWORD:-}"
ASSETS=()

# ---- arg parse ----
for arg in "$@"; do
  case "$arg" in
    --ch-host=*)     CH_HOST="${arg#*=}" ;;
    --ch-user=*)     CH_USER="${arg#*=}" ;;
    --ch-password=*) CH_PASSWORD="${arg#*=}" ;;
    --db=*)          DB="${arg#*=}" ;;
    --cluster=*)     CLUSTER="${arg#*=}" ;;
    --*) echo "ERROR: unknown flag: $arg" >&2; exit 2 ;;
    *)   ASSETS+=("$arg") ;;
  esac
done

: "${CH_HOST:?--ch-host required}"
: "${CH_USER:?--ch-user required}"

# Resolve the repo root (this script lives in scripts/) so the bundle paths
# in lib.sh resolve regardless of the caller's cwd.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

# Shared with install.sh: setup_netrc, ch_file_upload, bundle_concat,
# the canonical bundle lists, and push_asset. See scripts/lib.sh.
source "$SCRIPT_DIR/lib.sh"
setup_netrc "$CH_HOST" "$CH_USER" "$CH_PASSWORD"

# No asset args → push the whole SPA set (both bundles + verbatim files).
if [[ ${#ASSETS[@]} -eq 0 ]]; then
  ASSETS=(charts.js dash.js "${VERBATIM_ASSETS[@]}")
fi

echo "==> bentoclick asset update"
echo "    CH:      $CH_HOST"
echo "    DB:      $DB"
echo "    cluster: $CLUSTER"
echo "    assets:  ${ASSETS[*]}"

for a in "${ASSETS[@]}"; do
  echo "==> pushing $a"
  push_asset "$a"
done

echo "==> done. Asset bytes rotate on the next page load (no-cache/no-store headers)."
