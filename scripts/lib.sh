#!/usr/bin/env bash
# bentoclick deploy library — shared helpers for scripts/install.sh and
# scripts/update.sh. This file is *sourced*, never executed directly.
#
# Contract for callers:
#   - cd to the repo root before sourcing (the bundle paths below are
#     repo-root-relative);
#   - set DB and CLUSTER (ch_file_upload reads them);
#   - call setup_netrc <ch-host> <ch-user> <ch-password> before any
#     ch_query / ch_file_upload / push_asset.

# ---- ClickHouse auth + query --------------------------------------------
# Transport: by default ch_query POSTs to the ClickHouse HTTP endpoint as the
# admin user (curl + .netrc). For clusters with no reachable admin HTTP endpoint
# (e.g. kubectl-only access), set CH_EXEC_CMD to a clickhouse-client wrapper
# such as `cl <cluster>`; ch_query then pipes the SQL to it instead. update.sh wires
# this from its --exec flag.
CH_EXEC_CMD=()

# Password is written to a per-run .netrc tempfile (chmod 600) rather than
# passed via --user, which would appear in `ps` for the duration of each
# curl. The tempfile is removed on EXIT.
setup_netrc() {
  CH_HOST="$1"; CH_USER="$2"; CH_PASSWORD="${3:-}"
  local host_for_netrc
  host_for_netrc="$(printf '%s' "$CH_HOST" | sed -e 's|^https*://||' -e 's|[:/].*$||')"
  NETRC_FILE="$(mktemp)"
  chmod 600 "$NETRC_FILE"
  printf 'machine %s\n  login %s\n  password %s\n' \
    "$host_for_netrc" "$CH_USER" "$CH_PASSWORD" > "$NETRC_FILE"
  trap 'rm -f "$NETRC_FILE"' EXIT
}

ch_curl() {
  curl -fsS --netrc-file "$NETRC_FILE" "$@"
}

ch_query() {
  # stdin = SQL; runs as the admin user. Over HTTP the endpoint requires
  # one statement per request, so callers that need multi-statement files
  # must split first (see install.sh's ch_apply_file). Over an exec wrapper
  # (--multiquery) one statement per call is still how callers invoke it.
  if [[ ${#CH_EXEC_CMD[@]} -gt 0 ]]; then
    "${CH_EXEC_CMD[@]}" --multiquery
  else
    ch_curl --data-binary @- "${CH_HOST}/"
  fi
}

# ---- asset upload (cluster fan-out) -------------------------------------
ch_file_upload() {
  # $1 = relative path inside user_files (e.g. dash/spa.js)
  # $2 = local file
  #
  # Cluster-distribution model: one File-engine table per asset, created
  # ON CLUSTER so every replica has its own table pointing at the SAME
  # absolute path on its OWN local user_files/ disk. `INSERT INTO FUNCTION
  # clusterAllReplicas(...)` then fans the bytes out to every replica's
  # local file(). (CH rejects table functions inside clusterAllReplicas,
  # so the File-engine table is the required indirection.)
  #
  # Each replica is a separate write; there is no quorum or atomic
  # rotation. During the brief inconsistency window an unlucky reader
  # behind a non-sticky LB might fetch stale bytes; acceptable for SPA
  # assets — the browser cache headers (no-store on spa.js, no-cache on
  # dash.js) re-fetch on next page load.
  local path="$1" local_file="$2"
  # Asset path → CH-safe table name: 'dash/spa.js' → '_asset_dash_spa_js'.
  local table_name
  table_name="_asset_$(printf '%s' "$path" | tr -c 'A-Za-z0-9_' '_')"
  local b64
  b64="$(base64 < "$local_file" | tr -d '\n')"
  printf "CREATE TABLE IF NOT EXISTS %s.%s ON CLUSTER '%s' (content String) ENGINE = File('RawBLOB', '/var/lib/clickhouse/user_files/%s')" \
    "$DB" "$table_name" "$CLUSTER" "$path" \
    | ch_query > /dev/null
  printf "INSERT INTO FUNCTION clusterAllReplicas('%s', '%s', '%s') SETTINGS engine_file_truncate_on_insert = 1 SELECT base64Decode('%s')" \
    "$CLUSTER" "$DB" "$table_name" "$b64" \
    | ch_query > /dev/null
}

# ---- JS bundling --------------------------------------------------------
# dash.js and charts.js source is split across runtime/v1/{core,panels,charts}/
# for human-readability, but the iframe boot fetches a single
# /lib/v1/dash.js and a single /lib/v1/charts.js. Concatenate each tree in
# topological order at deploy time and upload the resulting bundle.
#
# Order matters within each bundle: non-hoisted declarations (`const`,
# `class`) must precede any reference. Functions are hoisted.
bundle_concat() {
  # $1 = output tempfile, $2..$N = source files in topological order.
  local out="$1"; shift
  : > "$out"
  for src in "$@"; do
    if [[ ! -f "$src" ]]; then
      echo "ERROR: bundle source missing: $src" >&2
      return 1
    fi
    printf '\n// ==== bundle: %s ====\n' "$src" >> "$out"
    cat "$src" >> "$out"
  done
}

# Canonical bundle file order. install.sh and update.sh share these so a
# full install and a targeted asset push always produce identical bytes.
CHARTS_BUNDLE=(
  runtime/v1/charts/palette.js
  runtime/v1/charts/scales.js
  runtime/v1/charts/svg.js
  runtime/v1/charts.js
)
DASH_BUNDLE=(
  runtime/v1/core/fmt.js
  runtime/v1/core/interpolate.js
  runtime/v1/core/run-state.js
  runtime/v1/core/markdown.js
  runtime/v1/core/ledger.js
  runtime/v1/core/badge.js
  runtime/v1/core/csv.js
  runtime/v1/panels/_shared.js
  runtime/v1/panels/chart-helpers.js
  runtime/v1/panels/kpi-strip.js
  runtime/v1/panels/table.js
  runtime/v1/panels/bars.js
  runtime/v1/panels/markdown.js
  runtime/v1/panels/hero.js
  runtime/v1/panels/callouts.js
  runtime/v1/panels/html.js
  runtime/v1/panels/script.js
  runtime/v1/panels/line.js
  runtime/v1/panels/combo.js
  runtime/v1/panels/chart.js
  runtime/v1/panels/dataset.js
  runtime/v1/dash.js
)
# Runtime files served verbatim (no bundling). Names are the served
# basenames under dash/; sources live at runtime/v1/<name>.
VERBATIM_ASSETS=(
  spa.html
  spa.js
  spa-helpers.js
  tweaks.js
  dash-theme.css
  oauth-callback.html
)

# push_asset <name> — push one SPA asset by its served basename.
#   dash.js / charts.js → rebundle from source (above lists) then upload
#   <other>             → upload runtime/v1/<name> verbatim as dash/<name>
# Requires setup_netrc + DB/CLUSTER already set.
push_asset() {
  local name="$1" tmp
  case "$name" in
    dash.js)
      tmp="$(mktemp)"
      bundle_concat "$tmp" "${DASH_BUNDLE[@]}"
      ch_file_upload "dash/dash.js" "$tmp"
      rm -f "$tmp"
      ;;
    charts.js)
      tmp="$(mktemp)"
      bundle_concat "$tmp" "${CHARTS_BUNDLE[@]}"
      ch_file_upload "dash/charts.js" "$tmp"
      rm -f "$tmp"
      ;;
    *)
      if [[ -f "runtime/v1/$name" ]]; then
        ch_file_upload "dash/$name" "runtime/v1/$name"
      else
        echo "ERROR: unknown asset '$name' (expected dash.js/charts.js or a runtime/v1/<name>)" >&2
        return 1
      fi
      ;;
  esac
}
