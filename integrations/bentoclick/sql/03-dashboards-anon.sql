-- Rewire a bentoclick install (${DB}) into anonymized-authoring mode.
--
-- BEFORE (stock bentoclick):
--   dashboards_raw (Null, String)  --MV(sanitize,DEFINER)-->  dashboards (RRMT)   <- SPA reads
--
-- AFTER (anon mode):
--   dashboards_raw (Null, String)  --MV(sanitize,DEFINER)-->  dashboards_tok (RRMT, TOKENS)  <- LLM edit-loop reads
--                                                                   |
--                                                       dashboards (DEFINER VIEW, detok) <- SPA reads (UNCHANGED query)
--
-- The authoring artifact (dashboards_tok) is permanently tokenized, so the
-- LLM's read-modify-save loop always reads tokens and stays blind. The SPA
-- still does `FROM ${DB}.dashboards FINAL` verbatim — `dashboards` is now a
-- de-tokenizing DEFINER view whose INNER FINAL dedups the latest version
-- (the SPA's outer FINAL becomes a harmless no-op; verified on CH 26.3).
--
-- This is NOT "de-tok at save" (variant A): de-tok happens at view/read
-- time over the tokenized store, never written back, so the dictionary
-- stays authoritative and the loop never sees real names.
--
-- Apply AFTER 01-token-dict.sql and 02-detok-udf.sql. Parameters: ${DB}
-- (dashboard database), ${META_DB} (anond meta DB holding the dictionary),
-- ${CLUSTER} (CH cluster macro, default '{cluster}').

-- 1. Tokenized typed store — same shape/engine as stock bentoclick's
--    `dashboards`. This becomes the MV target and the LLM read-back surface.
CREATE TABLE IF NOT EXISTS ${DB}.dashboards_tok
  ON CLUSTER '{cluster}'
(
    slug         String,
    title        String,
    subtitle     String        DEFAULT '',
    concurrent   Bool          DEFAULT false,
    spec_version UInt8         DEFAULT 1,
    params       Array(JSON)   DEFAULT [],
    panels       Array(JSON)   DEFAULT [],
    meta         JSON          DEFAULT '{}',
    tags         Array(String) DEFAULT [],
    owner        String        DEFAULT currentUser(),
    updated_at   DateTime      DEFAULT now()
)
ENGINE = ReplicatedReplacingMergeTree(updated_at)
ORDER BY (owner, slug);

-- 2. One-time migration of any existing rows from the old read target into
--    the tokenized store. Safe for either content: token specs copy as-is;
--    pre-existing REAL specs copy too and the de-tok view no-ops over them
--    (no tokens -> nothing to expand). Run ONCE; re-running just refreshes
--    updated_at (RRMT de-dupes on (owner, slug)).
INSERT INTO ${DB}.dashboards_tok
SELECT * FROM ${DB}.dashboards;

-- 3. Re-point the sanitize MV at the tokenized store. The SELECT body MUST
--    match your installed bentoclick version EXACTLY except for the TO
--    target; reproduced here from v0.1.0 (schema/01-database.sql). If your
--    bentoclick differs, copy its dashboards_mv body and change only TO.
DROP VIEW IF EXISTS ${DB}.dashboards_mv ON CLUSTER '{cluster}';
CREATE MATERIALIZED VIEW IF NOT EXISTS ${DB}.dashboards_mv
  ON CLUSTER '{cluster}'
  TO ${DB}.dashboards_tok
  DEFINER = ${DB}_definer
  SQL SECURITY DEFINER AS
SELECT
    slug,
    title,
    subtitle,
    concurrent,
    spec_version,
    JSONExtract(params,                          'Array(JSON)')   AS params,
    JSONExtract(sanitize_json_text(panels),      'Array(JSON)')   AS panels,
    CAST(meta AS JSON)                                            AS meta,
    JSONExtract(tags,                            'Array(String)') AS tags,
    currentUser()                                                 AS owner,
    now()                                                         AS updated_at
FROM ${DB}.dashboards_raw;

-- 4. Replace the `dashboards` TABLE with the de-tokenizing DEFINER VIEW.
--    DANGER: this drops the old read-target table. Step 2 already copied its
--    rows into dashboards_tok, so no data is lost; the view re-derives the
--    SPA-facing rows from there. The view runs as ${DB}_definer so viewers
--    never need dictionary access — they only ever see de-tokenized specs.
--    Whole-spec de-tok (title/subtitle/params/panels/meta): a token can
--    appear in any author-written field, and reserved-namespace makes a
--    global expand safe.
DROP TABLE IF EXISTS ${DB}.dashboards ON CLUSTER '{cluster}';
CREATE VIEW ${DB}.dashboards
  ON CLUSTER '{cluster}'
  DEFINER = ${DB}_definer
  SQL SECURITY DEFINER AS
SELECT
    slug,
    detok(title)                                            AS title,
    detok(subtitle)                                         AS subtitle,
    concurrent,
    spec_version,
    JSONExtract(detok(toJSONString(params)), 'Array(JSON)') AS params,
    JSONExtract(detok(toJSONString(panels)), 'Array(JSON)') AS panels,
    CAST(detok(toJSONString(meta)) AS JSON)                 AS meta,
    tags,
    owner,
    updated_at
FROM ${DB}.dashboards_tok FINAL;

-- 5. Grants live in 00-anon-rbac.sql (applied first): the definer's
--    SELECT dashboards_tok + dictGet token_to_real, the LLM anon_author_role,
--    and the viewer role. Keeping all RBAC in one file is deliberate — see
--    docs/RBAC.md for the full principal × privilege matrix.
