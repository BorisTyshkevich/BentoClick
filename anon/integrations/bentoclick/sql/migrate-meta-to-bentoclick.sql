-- Phase 1f: retire the legacy `altinity` meta DB.
--   1. Move anond's internal discovery meta (profile_*, generated_objects,
--      manifest) into `bentoclick` (co-located with the LLM-facing registry).
--   2. Drop the dead altinity guide views (MCP repointed to bentoclick).
--   3. Drop the deprecated oauth_refresh_* KeeperMap tables (old altinity-mcp
--      broker experiment — no longer used).
--   4. Drop the now-empty `altinity` database.
-- anond's --meta-db default is now `bentoclick` (cmd/anond/main.go). The de-anon
-- secret already lives in `bentosecrets.*`. Data preserved via RENAME.

DROP VIEW IF EXISTS altinity.schema_guide;
DROP VIEW IF EXISTS altinity.attr_guide;

RENAME TABLE
  altinity.generated_objects     TO bentoclick.generated_objects,
  altinity.manifest              TO bentoclick.manifest,
  altinity.profile_attr_keys     TO bentoclick.profile_attr_keys,
  altinity.profile_catalog       TO bentoclick.profile_catalog,
  altinity.profile_columns       TO bentoclick.profile_columns,
  altinity.profile_conventions   TO bentoclick.profile_conventions,
  altinity.profile_hot_columns   TO bentoclick.profile_hot_columns,
  altinity.profile_queries       TO bentoclick.profile_queries,
  altinity.profile_relations     TO bentoclick.profile_relations,
  altinity.profile_shape         TO bentoclick.profile_shape,
  altinity.profile_verification  TO bentoclick.profile_verification,
  altinity.profile_workload      TO bentoclick.profile_workload;

DROP TABLE IF EXISTS altinity.oauth_refresh_consumed_jtis SYNC;
DROP TABLE IF EXISTS altinity.oauth_refresh_revoked_families SYNC;

DROP DATABASE IF EXISTS altinity SYNC;
