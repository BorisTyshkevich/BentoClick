-- anond × bentoclick — RBAC for the de-tok dictionary + the LLM/viewer split.
-- Apply on OTEL (where bentoclick + real data live) BEFORE 01/02/03.
-- See docs/RBAC.md for the full model and the threat analysis.
--
-- Params: ${DB} (dashboard DB, e.g. bentoclick), ${META_DB} (anond meta +
-- LLM-facing registry DB, e.g. bentoclick), ${SECRET_DB} (de-anon secret DB
-- holding identifier_map + the token_to_real dict, e.g. bentosecrets — MUST be
-- isolated from any "read all dashboards" grant), ${DATA_DB} (real data DB,
-- e.g. claude_otel), ${CLUSTER}, ${DICT_READER_PW} (a secret you supply; rotate
-- like any DB credential).

-- 1. Dictionary-reader user. The ONLY principal that reads the de-anon map
--    (besides the definer's dictGet). SELECT on identifier_map and nothing
--    else — DEFAULT ROLE NONE so it never inherits a broader role.
CREATE USER IF NOT EXISTS anon_dict_reader
  ON CLUSTER '{cluster}'
  IDENTIFIED WITH sha256_password BY '${DICT_READER_PW}'
  DEFAULT ROLE NONE;
GRANT SELECT ON ${SECRET_DB}.identifier_map TO anon_dict_reader
  ON CLUSTER '{cluster}';

-- 2. Named collection feeding token_to_real's CLICKHOUSE source. Credentials
--    live HERE (server-side), not in CREATE DICTIONARY — so SHOW CREATE
--    DICTIONARY leaks neither the password nor real names (verified, CH 26.3).
--    port/secure: localhost native 9000 works for a same-node source; a
--    hardened/secure cluster may need `port = 9440, secure = 1`.
CREATE NAMED COLLECTION IF NOT EXISTS anon_dict_src
  ON CLUSTER '{cluster}' AS
  host     = 'localhost',
  port     = 9000,
  user     = 'anon_dict_reader',
  password = '${DICT_READER_PW}',
  db       = '${SECRET_DB}';

-- 3. bentoclick definer — resolves tokens inside the SECURITY DEFINER de-tok
--    view, and reads the tokenized store. This is the only token-resolving
--    grant any login-capable principal holds.
GRANT SELECT  ON ${DB}.dashboards_tok        TO ${DB}_definer ON CLUSTER '{cluster}';
GRANT dictGet ON ${SECRET_DB}.token_to_real  TO ${DB}_definer ON CLUSTER '{cluster}';

-- 4. LLM authoring role. Writes token specs and reads them back (TOKENS) for
--    the edit loop. It must never resolve a token or touch real data, so it
--    gets exactly two grants and explicit defensive REVOKEs.
--    NOTE: do NOT grant stock bentoclick's ${DB}_writer_role/${DB}_reader_role
--    here — reader_role's `SELECT ON ${DB}.*` now covers the de-tok view
--    `dashboards` (real names). anon mode splits that role in two (this one
--    + the viewer role below).
CREATE ROLE IF NOT EXISTS anon_author_role ON CLUSTER '{cluster}';
GRANT INSERT(slug, title, subtitle, concurrent, spec_version, params, panels, meta, tags)
   ON ${DB}.dashboards_raw TO anon_author_role ON CLUSTER '{cluster}';
GRANT SELECT ON ${DB}.dashboards_tok TO anon_author_role ON CLUSTER '{cluster}';
-- defense in depth against grant drift (REVOKE of an un-granted priv is a no-op)
REVOKE SELECT  ON ${DB}.dashboards            FROM anon_author_role ON CLUSTER '{cluster}';
REVOKE dictGet ON ${SECRET_DB}.token_to_real  FROM anon_author_role ON CLUSTER '{cluster}';
REVOKE SELECT  ON ${SECRET_DB}.*              FROM anon_author_role ON CLUSTER '{cluster}';
REVOKE SELECT  ON ${META_DB}.*                FROM anon_author_role ON CLUSTER '{cluster}';
REVOKE SELECT  ON ${DATA_DB}.*                FROM anon_author_role ON CLUSTER '{cluster}';

-- 5. Viewer role. Reads the de-tok view (real SQL) + the prefix view; the SPA
--    runs each panel's SQL as the viewer's OWN JWT identity, so real-data
--    access is gated by THEIR existing ${DATA_DB} grants (assigned per-user,
--    not here). No access to the tokenized tier or any secret.
CREATE ROLE IF NOT EXISTS ${DB}_anon_viewer_role ON CLUSTER '{cluster}';
GRANT SELECT ON ${DB}.dashboards        TO ${DB}_anon_viewer_role ON CLUSTER '{cluster}';
GRANT SELECT ON ${DB}.dashboards_prefix TO ${DB}_anon_viewer_role ON CLUSTER '{cluster}';
REVOKE SELECT  ON ${DB}.dashboards_tok        FROM ${DB}_anon_viewer_role ON CLUSTER '{cluster}';
REVOKE SELECT  ON ${DB}.dashboards_raw        FROM ${DB}_anon_viewer_role ON CLUSTER '{cluster}';
REVOKE dictGet ON ${SECRET_DB}.token_to_real  FROM ${DB}_anon_viewer_role ON CLUSTER '{cluster}';
REVOKE SELECT  ON ${SECRET_DB}.*              FROM ${DB}_anon_viewer_role ON CLUSTER '{cluster}';
REVOKE SELECT  ON ${META_DB}.*                FROM ${DB}_anon_viewer_role ON CLUSTER '{cluster}';
