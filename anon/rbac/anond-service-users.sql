-- anond service users — replace the admin (clickhouse_operator) identity the
-- CLI uses today with a dedicated least-privilege principal on each cluster.
-- This is the identity the future in-MCP discovery job reuses (DESIGN.md §2.1).
-- See docs/RBAC.md for the rationale and the one broad grant (CREATE DATABASE).
--
-- Params: ${META_DB} (bentoclick — anond meta + LLM-facing registry),
-- ${SECRET_DB} (bentosecrets — de-anon secret: identifier_map, masking_plan,
-- token_to_real dict), ${DATA_DB} (mydb — source only), ${CLUSTER},
-- ${ANOND_PW}.

-- ====================================================================
-- SOURCE cluster: reads system.* + real data, writes the de-anon
-- secret (identifier_map, masking_plan) to ${SECRET_DB} and the tokens-only
-- profile meta to ${META_DB}. NO real-data writes, NOT admin.
-- Pre-create ${META_DB} and ${SECRET_DB} as admin once, so anond needs no
-- CREATE DATABASE on the cluster that holds real data (the tighter posture).
-- If your anond build still issues `CREATE DATABASE IF NOT EXISTS` at startup,
-- either pre-create them AND grant CREATE TABLE only (CH treats the no-op
-- create as allowed when the DB exists), or temporarily add
-- `GRANT CREATE DATABASE ON *.*` for the first run.
CREATE USER IF NOT EXISTS anond
  ON CLUSTER '{cluster}'
  IDENTIFIED WITH sha256_password BY '${ANOND_PW}'
  DEFAULT ROLE NONE;

GRANT SELECT ON system.* TO anond ON CLUSTER '{cluster}';
GRANT SELECT ON ${DATA_DB}.* TO anond ON CLUSTER '{cluster}';   -- mining, masking SELECTs, sampling
GRANT SELECT, INSERT, CREATE TABLE, CREATE DICTIONARY, ALTER, DROP TABLE
   ON ${META_DB}.* TO anond ON CLUSTER '{cluster}';
-- the de-anon secret tables (identifier_map, masking_plan) land here, isolated
-- from the LLM-facing ${META_DB}/registry; anond writes them via SecretStore.
GRANT SELECT, INSERT, CREATE TABLE, ALTER, DROP TABLE
   ON ${SECRET_DB}.* TO anond ON CLUSTER '{cluster}';
-- explicitly NOT: write on ${DATA_DB}, CREATE/DROP DATABASE (DBs pre-created),
-- access management, or anything on other databases.

-- ====================================================================
-- DEST cluster (demo/sandbox): no real data exists here. anond creates
-- per-run sandbox databases with token names + writes the tokens-only
-- profile meta. CREATE DATABASE is required (sandbox DB names are dynamic);
-- this is the one unavoidable broad grant. Foreign-object safety is enforced
-- in anond's code (generated_objects registry + ensureOurs abort), NOT by
-- RBAC — see docs/RBAC.md. Run this block against the DEST cluster.
--
-- CREATE USER IF NOT EXISTS anond ON CLUSTER '{cluster}'
--   IDENTIFIED WITH sha256_password BY '${ANOND_PW}' DEFAULT ROLE NONE;
-- GRANT CREATE DATABASE ON *.* TO anond ON CLUSTER '{cluster}';
-- GRANT SELECT, INSERT, CREATE TABLE, DROP TABLE, DROP DATABASE ON *.*
--    TO anond ON CLUSTER '{cluster}';   -- scoped-by-code, not by RBAC
-- GRANT SELECT ON system.* TO anond ON CLUSTER '{cluster}';
-- (left commented: apply deliberately against the sandbox cluster, whose
--  admin context differs from the source.)
