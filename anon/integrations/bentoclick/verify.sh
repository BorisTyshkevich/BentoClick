#!/usr/bin/env bash
# Acceptance probes for an anon bentoclick deployment — the security checks that
# the retired system-anon/verify.sh used to run, generalized to the registry +
# bentosecrets split and runnable against any sandbox. Each probe is a query that
# must return 0; the script exits non-zero if any fails. Run after a sandbox build
# / RBAC change. Read-only.
#
#   CL="cl otel" ANON_ROLE=anon_mcp_reader VIEWER_ROLE=bentoclick_anon_viewer_role \
#   SECRET_DB=bentosecrets ./verify.sh [anon_database ...]
#
# Layer note: these probe DEPLOYMENT objects/roles (bentoclick.*, bentosecrets.*,
# anon_mcp_reader, …) — they are NOT in anond's generic `verify` (which checks the
# model-agnostic no-survivor / sandbox-DDL / trusted-split invariants).
set -uo pipefail
CL="${CL:-cl otel}"
ANON_ROLE="${ANON_ROLE:-anon_mcp_reader}"
VIEWER_ROLE="${VIEWER_ROLE:-bentoclick_anon_viewer_role}"   # created by 00-anon-rbac.sql as ${DB}_anon_viewer_role
SECRET_DB="${SECRET_DB:-bentosecrets}"
REG_DB="${REG_DB:-bentoclick}"
DATA_DBS="${DATA_DBS:-claude_otel}"   # real (non-anon) data DBs the anon role must not reach
fail=0
ck() { # ck "label" "query-returning-0-for-pass"
  local got; got="$($CL --query "$2" 2>&1)"
  if [ "$got" = "0" ]; then echo "PASS  $1"; else echo "FAIL  $1  (got: $got)"; fail=1; fi
}

echo "== A. anon role isolation: no grant on the de-anon secret or real data =="
ck "anon role no SELECT on ${SECRET_DB}.*" \
  "SELECT count() FROM system.grants WHERE role_name='${ANON_ROLE}' AND database='${SECRET_DB}'"
ck "anon role no dictGet on token_to_real" \
  "SELECT count() FROM system.grants WHERE role_name='${ANON_ROLE}' AND access_type='dictGet'"
for d in ${DATA_DBS}; do
  ck "anon role no SELECT on real ${d}" \
    "SELECT count() FROM system.grants WHERE role_name='${ANON_ROLE}' AND access_type='SELECT' AND database='${d}'"
done
ck "anon role no SELECT on real system" \
  "SELECT count() FROM system.grants WHERE role_name='${ANON_ROLE}' AND access_type='SELECT' AND database='system'"
ck "anon role no SELECT on ${REG_DB}.dashboards (de-tok view)" \
  "SELECT count() FROM system.grants WHERE role_name='${ANON_ROLE}' AND access_type='SELECT' AND database='${REG_DB}' AND table='dashboards'"

echo "== B. viewer isolation: cannot read the tokenized store or the secret =="
ck "viewer no SELECT on ${REG_DB}.dashboards_tok" \
  "SELECT count() FROM system.grants WHERE role_name='${VIEWER_ROLE}' AND access_type='SELECT' AND database='${REG_DB}' AND table='dashboards_tok'"
ck "viewer no SELECT on ${REG_DB}.dashboards_raw" \
  "SELECT count() FROM system.grants WHERE role_name='${VIEWER_ROLE}' AND access_type='SELECT' AND database='${REG_DB}' AND table='dashboards_raw'"
ck "viewer no grant on ${SECRET_DB}.*" \
  "SELECT count() FROM system.grants WHERE role_name='${VIEWER_ROLE}' AND database='${SECRET_DB}'"

echo "== C. detok round-trips known tokens to real values =="
# Model-agnostic: take real tokens from the secret map (admin-only) and confirm
# none stay token-shaped after detok (every token resolves to a real value).
ck "detok resolves db_/tbl_/col_ tokens (none stay tokenized)" \
  "SELECT countIf(match(detok(token), '^(db|tbl|col|user|host|dict|field)_[0-9a-f]+\$')) FROM (SELECT DISTINCT token FROM ${SECRET_DB}.identifier_map WHERE match(token, '^(db|tbl|col)_[0-9a-f]+\$') LIMIT 100)"

echo "== D. de-anon secret does not expose credentials in DDL =="
ck "token_to_real SHOW CREATE has no inline password" \
  "SELECT toUInt8(positionCaseInsensitive((SELECT create_table_query FROM system.tables WHERE database='${SECRET_DB}' AND name='token_to_real'), 'password') > 0)"

echo "== E. no-survivor (model-aware via the registry's naming) =="
# Tokenizing sandboxes: no REAL table name may survive — every MergeTree table
# must be token-named (tbl_<hex>).
for db in $($CL --query "SELECT DISTINCT anon_database FROM ${REG_DB}.schema_guide WHERE naming='tokens'" 2>/dev/null); do
  ck "$db (tokenizing): all MergeTree table names are tokens" \
    "SELECT countIf(NOT match(name, '^tbl_[0-9a-f]+\$')) FROM system.tables WHERE database='$db' AND engine LIKE '%MergeTree%'"
done
# Schema-preserving sandboxes: real names, but identifier-array columns must hold
# only namespace tokens (query_log.databases is the representative case).
for db in $($CL --query "SELECT DISTINCT anon_database FROM ${REG_DB}.schema_guide WHERE naming='real'" 2>/dev/null); do
  if [ "$($CL --query "EXISTS TABLE $db.query_log" 2>/dev/null)" = "1" ]; then
    ck "$db (schema-preserving): query_log.databases all db_ tokens" \
      "SELECT countIf(notEmpty(d) AND NOT match(d, '^db_[0-9a-f]+\$')) FROM $db.query_log ARRAY JOIN databases AS d"
  fi
done

echo "== F. P1 guard: identity attr-key values are NOT kept real (tokenizing sandbox) =="
ck "no identity attr-key marked kept-real in the registry" \
  "SELECT count() FROM ${REG_DB}.attr_guide WHERE role='identity' AND usage LIKE '%filter%'"

echo
if [ "$fail" = 0 ]; then echo "ALL PROBES PASSED"; else echo "SOME PROBES FAILED"; fi
exit $fail
