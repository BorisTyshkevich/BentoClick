-- bentoclick — SQL SECURITY DEFINER user for dashboards_mv.
--
-- The materialized view in 01-database.sql runs as this user
-- (`DEFINER = ${DB}_definer SQL SECURITY DEFINER`), regardless of
-- who triggered the INSERT into dashboards_raw. Definer-mode means:
--
--   * The MV's destination INSERT into ${DB}.dashboards runs with
--     this user's privileges, NOT the inserter's. We can revoke
--     INSERT on `dashboards` from every other role without breaking
--     the legitimate MV-driven write path.
--
--   * `currentUser()` inside the MV would return this definer; to
--     record the actual session-initiating user as `owner`, the MV
--     uses `initialUser()` (see 01-database.sql).
--
-- The user is created passwordless (`IDENTIFIED WITH no_password`)
-- because it never logs in interactively — CH only references it by
-- name from the MV's DEFINER clause.
--
-- Filename starts with `00-` so scripts/install.sh's sorted apply order
-- creates this user BEFORE 01-database.sql's MV references it. The
-- definer's grants on `${DB}.dashboards_raw` and `${DB}.dashboards`
-- live at the END of 01-database.sql so they apply once those
-- tables exist.

CREATE USER IF NOT EXISTS ${DB}_definer
  ON CLUSTER '{cluster}'
  IDENTIFIED WITH no_password;
