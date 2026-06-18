-- One-time migration: move the de-anon secret OUT of the dashboards DB (M1).
--
-- Before the SecretStore fix (commit 825c406), the tokenizing anond model wrote
-- identifier_map + masking_plan to MetaDB (bentoclick) — co-located with the
-- dashboards and reachable by bentoclick_reader_role's `SELECT bentoclick.*`.
-- The secret belongs only in the SECRET_DB (bentosecrets), which is locked to
-- anon_dict_reader (SELECT identifier_map) + bentoclick_definer (dictGet) and is
-- where the token_to_real dict already sources from. This backfills any
-- bentoclick-only rows into bentosecrets, reloads the dict, then drops the
-- bentoclick copies.
--
-- Apply AS ADMIN on the source cluster. Idempotent: tokens are
-- HMAC-deterministic, so re-inserts dedup by token in the dict's GROUP BY.
-- ORDER MATTERS: verify de-tok (step 3) BEFORE the DROP (step 4).
-- The meta tables are plain (non-replicated) ReplacingMergeTree, hence no
-- ON CLUSTER. Names hardcoded to the live deployment (bentoclick/bentosecrets).

-- 1. Backfill the trusted tables into the secret DB (union; ReplacingMergeTree).
INSERT INTO bentosecrets.identifier_map SELECT * FROM bentoclick.identifier_map;
INSERT INTO bentosecrets.masking_plan   SELECT * FROM bentoclick.masking_plan;

-- 2. Refresh the dict from its (bentosecrets) source.
SYSTEM RELOAD DICTIONARY bentosecrets.token_to_real;

-- 3. >>> VERIFY before proceeding <<< bentoclick.dashboards FINAL must show 0
--    residual tbl_/col_/field_/_anon tokens (CL="cl <cluster>" verify.sh, all pass).

-- 4. Drop the redundant secret copies from the dashboards DB (closes M1).
DROP TABLE IF EXISTS bentoclick.identifier_map;
DROP TABLE IF EXISTS bentoclick.masking_plan;

-- 5. Defense-in-depth: no LLM/viewer principal may reach the secret DB
--    (REVOKE of an un-granted privilege is a no-op).
REVOKE SELECT  ON bentosecrets.*             FROM anon_author_role;
REVOKE dictGet ON bentosecrets.token_to_real FROM anon_author_role;
REVOKE SELECT  ON bentosecrets.*             FROM bentoclick_viewer_role;
REVOKE dictGet ON bentosecrets.token_to_real FROM bentoclick_viewer_role;
