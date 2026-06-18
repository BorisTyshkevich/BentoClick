-- bentoclick — dashboard storage layer.
--
-- ${DB} is the dashboard database (default: `bentoclick`); ${SPA_ORIGIN}
-- is the public origin of the ClickHouse HTTPS server hosting the SPA.
-- Both are substituted by `scripts/install.sh` (or `tests/conftest.py` in test
-- runs) before this file is applied.
--
-- Three storage objects make up the dashboard write path:
--
--   dashboards_raw  Null engine — write target for tools/agents.
--                   Payload columns (params, panels, meta, tags) are
--                   stored as STRING (JSON-encoded text) so the
--                   altinity-mcp reflector — which intentionally skips
--                   JSON / Array(JSON) types — sees plain string
--                   parameters that map cleanly to MCP tool args.
--                                |
--                                v
--   dashboards_mv   Materialized view, `SQL SECURITY DEFINER`. The MV
--                   runs as ${DB}_definer (see 00-definer.sql), which
--                   is the ONLY user with INSERT on `dashboards`. This
--                   moves the write-path security check from "every
--                   role omits INSERT on dashboards" (convention) to
--                   "the definer is the sole grant holder" (enforced
--                   at SHOW GRANTS time). `initialUser()` records the
--                   actual session user as `owner` despite the
--                   definer context.
--                                |
--                                v
--   dashboards     ReplicatedReplacingMergeTree — read target for the
--                                       SPA. Keeps structured types
--                                       (Array(JSON), JSON,
--                                       Array(String)) for fast
--                                       column access from the SPA's
--                                       SELECT.
--
-- Direct INSERT into `dashboards` is denied to every role (explicit
-- REVOKE in 02-roles.sql, defense in depth on top of the absence of
-- a grant). The only writer is the SECURITY DEFINER MV.
--
-- Cluster-ready: all DDL carries `ON CLUSTER '{cluster}'` and the
-- replicated engine uses default Zoo paths derived from the
-- `{database}/{table}/{shard}/{replica}` macros (configured on the
-- live antalya cluster; configured in `tests/clickhouse-config.xml`
-- for the test image).

CREATE DATABASE IF NOT EXISTS ${DB}
  ON CLUSTER '{cluster}';

-- Read target. Structured payload columns + owner/updated_at supplied
-- by the MV. owner and updated_at are DEFAULT (not MATERIALIZED) so
-- the MV's SELECT can supply them explicitly; column-level grants on
-- `_raw` prevent forgery (no INSERT(owner) is ever granted anywhere),
-- and the explicit REVOKE in 02-roles.sql blocks direct INSERTs on
-- this table even from roles that pick up a default INSERT grant.
CREATE TABLE IF NOT EXISTS ${DB}.dashboards
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

-- Write target. All payload columns are String — JSON-encoded text the
-- agent provides directly. The MV does the conversion to structured
-- types on the way to `dashboards`. Defaults are empty JSON literals
-- so an agent that omits an optional column still produces a valid
-- stored row after the MV parse. Null engine — no actual storage,
-- replication, or replica state needed.
CREATE TABLE IF NOT EXISTS ${DB}.dashboards_raw
  ON CLUSTER '{cluster}'
(
    slug         String,
    title        String,
    subtitle     String DEFAULT '',
    concurrent   Bool   DEFAULT false,
    spec_version UInt8  DEFAULT 1,
    params       String DEFAULT '[]',
    panels       String DEFAULT '[]',
    meta         String DEFAULT '{}',
    tags         String DEFAULT '[]'
)
ENGINE = Null;

-- sanitize_json_text — strip dangerous HTML constructs from a JSON
-- text blob. Applied to `panels` in the MV before JSONExtract'ing
-- into Array(JSON). Operating on the raw String form means we run
-- two regex passes per row, not one per panel — and the function
-- name generalises if we ever extend sanitization to other JSON
-- columns.
--
-- The deletions only affect *contents* of JSON string values (HTML
-- inside a panel's `html` field, for example); structural quotes and
-- braces are preserved, so the result remains valid JSON.
--
-- Threats handled in one alternation pass (case-insensitive, single
-- line), plus a second pass that neutralises `javascript:` URLs:
--
--   Paired elements (whole element stripped):
--     <script>…</script>, <iframe>…</iframe>, <object>…</object>,
--     <svg>…</svg>, <math>…</math>, <style>…</style>, <form>…</form>
--   Self-closing / orphan / void elements (opening tag stripped):
--     script, iframe, object, svg, math, style, form, embed, link,
--     base, meta
--   Event handlers:
--     on*="…", on*='…', and (the regex gap that drove this rewrite)
--     unquoted on*=value forms like `<img onerror=alert(1)>`.
--   URL scheme:
--     javascript:
--
-- RE2 (ClickHouse's regex engine) has no backreferences, which is
-- why paired-element patterns are enumerated individually rather
-- than `<(\w+)>…</\1>`.
--
-- Pattern detail: agent-provided JSON encoders (JavaScript's
-- JSON.stringify, Python's json.dumps) leave `/` unescaped, while
-- ClickHouse's toJSONString() escapes `/` as `\/`. Close-tag and
-- self-close patterns tolerate an optional leading backslash before
-- each `/` so both forms are covered; the double-quoted event
-- handler variant likewise tolerates an optional escape on the
-- surrounding quotes.
--
-- Pattern (one alternation, single line — ClickHouse SQL doesn't allow
-- adjacent string-literal concatenation):
--   Paired containers: <script|iframe|object|svg|math|style|form>…</…>
--   Orphan/self-closing/void: same set plus embed/link/base/meta
--   Event handlers: on*="…", on*='…', and unquoted on*=value
-- A second pass neutralises the `javascript:` URL scheme.
-- NB: no `ON CLUSTER '{cluster}'` here — CH 26.1 rejects it for
-- `CREATE FUNCTION` ("user-defined functions are replicated
-- automatically"). The function is propagated via Keeper just like
-- access management is.
CREATE OR REPLACE FUNCTION sanitize_json_text AS (s) ->
    replaceRegexpAll(
        replaceRegexpAll(
            s,
            '(?is)<script\\b[^>]*>.*?<\\\\?/script[^>]*>|<iframe\\b[^>]*>.*?<\\\\?/iframe[^>]*>|<object\\b[^>]*>.*?<\\\\?/object[^>]*>|<svg\\b[^>]*>.*?<\\\\?/svg[^>]*>|<math\\b[^>]*>.*?<\\\\?/math[^>]*>|<style\\b[^>]*>.*?<\\\\?/style[^>]*>|<form\\b[^>]*>.*?<\\\\?/form[^>]*>|<(?:script|iframe|object|svg|math|style|form|embed|link|base|meta)\\b[^>]*\\\\?/?>|\\bon[a-z]+\\s*=\\s*(?:\\\\?"[^"]*\\\\?"|''[^'']*''|[^\\s>"'']+)',
            ''),
        '(?i)javascript:', '');

-- The MV parses the agent-supplied JSON text into the read target's
-- structured types. Sanitization runs once over the panels text
-- before JSONExtract — single regex pass per row, no per-element
-- stringify/parse hops. The other JSON columns (params, meta, tags)
-- are parsed directly; they don't contain user-supplied HTML.
--
-- SQL SECURITY DEFINER: the destination INSERT into `${DB}.dashboards`
-- runs with ${DB}_definer's privileges (the only principal with
-- INSERT on the read target). Empirically verified in CH 26.3:
-- `currentUser()` inside a SQL SECURITY DEFINER MV returns the
-- session-initiating user, NOT the definer — so owner correctly
-- captures the actual inserter while the definer's grant is what
-- allows the write to proceed.
CREATE MATERIALIZED VIEW IF NOT EXISTS ${DB}.dashboards_mv
  ON CLUSTER '{cluster}'
  TO ${DB}.dashboards
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

-- dashboards_prefix — returns the SPA URL prefix the caller should
-- append a slug to in order to produce a share URL. Owner segment is
-- the URL-encoded full email so distinct identities never collide on
-- the same path (e.g. foo@altinity.com vs foo@gmail.com).
-- MCP-attached agents MUST read spa_origin / my_dashboards_prefix
-- from here rather than guessing — the MCP origin and the SPA origin
-- are different hosts.
CREATE OR REPLACE VIEW ${DB}.dashboards_prefix
  ON CLUSTER '{cluster}' AS
SELECT
    currentUser()                                              AS owner,
    '${SPA_ORIGIN}'                                            AS spa_origin,
    concat('${SPA_ORIGIN}', '/b/',
           encodeURLComponent(currentUser()), '/')             AS my_dashboards_prefix;

DROP VIEW IF EXISTS ${DB}.whoami ON CLUSTER '{cluster}';

-- Definer-user grants. Live at the END of this file so the tables
-- they reference already exist. The definer needs to SELECT from
-- _raw (the MV's source) and INSERT into dashboards (the MV's
-- destination). Nothing else — no DELETE, no ALTER, no read access
-- to the destination.
GRANT SELECT ON ${DB}.dashboards_raw TO ${DB}_definer
  ON CLUSTER '{cluster}';
GRANT INSERT ON ${DB}.dashboards TO ${DB}_definer
  ON CLUSTER '{cluster}';
