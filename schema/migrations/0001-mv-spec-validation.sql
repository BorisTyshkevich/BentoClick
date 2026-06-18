-- Migration 0001 — add spec-contract validation to dashboards_mv (issue #16).
--
-- For an EXISTING bentoclick install. `install.sh` re-runs are a no-op on an
-- already-created MV (`CREATE … IF NOT EXISTS`), so an existing cluster picks up
-- the validating MV only via this migration. Works for BOTH a plain install
-- (dashboards_mv → dashboards) and an anonymized-authoring install
-- (dashboards_mv → dashboards_tok, with the read-time detok view): MODIFY QUERY
-- swaps only the SELECT and preserves whatever TO target the MV already has, so
-- one migration covers both shapes.
--
-- What it does: swap the dashboards_mv query for the WHERE-gated validating body
-- (identical to schema/01-database.sql). The gates parse the spec inline and
-- throwIf() on anything outside the known contract: empty slug/title, spec_version
-- ceiling (> MAX_KNOWN, currently 1), unknown panel/param types, a panel that sets
-- both query and source, an enum param without options.
--
-- Apply AS ADMIN on the target cluster, substituting ${DB} for the dashboard
-- database (default `bentoclick`) first, e.g.:
--   sed 's/${DB}/bentoclick/g' schema/migrations/0001-mv-spec-validation.sql \
--     | cl <cluster> --multiquery
--
-- Idempotent and non-disruptive: `ALTER TABLE … MODIFY QUERY` swaps the MV's SELECT
-- in place — no DROP, so there is no window where inserts stop propagating, and the
-- TO target + DEFINER are preserved. (CH 26.3 has no `CREATE OR REPLACE MATERIALIZED
-- VIEW`; MODIFY QUERY is the in-place path. It works on both the new and the old
-- analyzer.) Re-running just re-applies the same body.
--
-- No CHECK constraints are added to dashboards_raw: ALTER ADD CONSTRAINT is
-- unsupported on the Null engine (CH 26.3, Code 48), so all validation lives in the
-- MV gates (which is also where slug/title non-emptiness is enforced).
--
-- NB: this MV body MUST stay byte-identical to schema/01-database.sql's
-- dashboards_mv body (a cross-check test guards the drift). If you bumped the known
-- type sets there, copy the updated body here too.

ALTER TABLE ${DB}.dashboards_mv ON CLUSTER '{cluster}' MODIFY QUERY
SELECT
    slug,
    title,
    subtitle,
    concurrent,
    spec_version,
    JSONExtract(params,                     'Array(JSON)')   AS params,
    JSONExtract(sanitize_json_text(panels), 'Array(JSON)')   AS panels,
    CAST(meta AS JSON)                                       AS meta,
    JSONExtract(tags,                       'Array(String)') AS tags,
    currentUser()                                            AS owner,
    now()                                                    AS updated_at
FROM ${DB}.dashboards_raw
WHERE throwIf(slug = '' OR title = '',
              'bentoclick: slug and title must be non-empty') = 0
  AND throwIf(spec_version = 0 OR spec_version > 1,
              'bentoclick: spec_version out of supported range [1..1]; declining a spec newer than this MV') = 0
  AND throwIf(NOT arrayAll(p ->
                JSONExtractString(toJSONString(p), 'type') IN
                  ('kpi-strip','table','bars','markdown','hero','callouts',
                   'html','script','line','combo','chart','dataset'),
                panels),
              'bentoclick: panel has an unknown or empty type') = 0
  AND throwIf(arrayExists(p ->
                JSONExtractString(toJSONString(p), 'query')  != ''
                AND JSONExtractString(toJSONString(p), 'source') != '',
                panels),
              'bentoclick: panel sets both query and source') = 0
  AND throwIf(NOT arrayAll(p ->
                JSONExtractString(toJSONString(p), 'name') != ''
                AND JSONExtractString(toJSONString(p), 'type') IN
                  ('int','enum','date','string')
                AND (JSONExtractString(toJSONString(p), 'type') != 'enum'
                     OR length(JSONExtractArrayRaw(toJSONString(p), 'options')) > 0),
                params),
              'bentoclick: invalid param (needs non-empty name, type in {int,enum,date,string}, enum needs options)') = 0;
