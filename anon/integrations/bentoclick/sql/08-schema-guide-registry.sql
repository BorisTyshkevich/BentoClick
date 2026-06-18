-- bentoclick.schema_guide / attr_guide — the multi-sandbox registry.
--
-- One row per (anon_database, table, column) describing every tokenized masked
-- sandbox the LLM may explore. Replaces the single-database altinity.schema_guide
-- / attr_guide views. Both producers write the backing `*_data` tables:
--   * anond --model=tokenizing        (token names, masked values)
--   * anond --model=schema-preserving  (real names, masked values)
--
-- Shape: a physical `*_data` ReplacingMergeTree (the producers INSERT here) +
-- a VIEW of the same name (the LLM-facing surface). The MCP discovers read
-- tools via `system.tables WHERE engine='View'`, so describe_schema /
-- describe_attributes MUST be VIEWS — the view also carries the COMMENT the
-- MCP serves as the tool description.
--
-- Tokens-only and LLM-facing by design: granted to the anon read role. The
-- de-anon SECRET (bentosecrets.identifier_map / token_to_real) is NOT here.

CREATE DATABASE IF NOT EXISTS bentoclick;

CREATE TABLE IF NOT EXISTS bentoclick.schema_guide_data
(
    run_id        String,
    anon_database String,                                   -- the FROM the LLM uses: mydb_anon, system_anon, …
    model         Enum8('tokenizing' = 1, 'schema-preserving' = 2),
    naming        Enum8('tokens' = 1, 'real' = 2),          -- are table/column NAMES tokens or real?
    table_name    String,                                   -- token (tokenizing) or real (schema-preserving)
    table_role    String  DEFAULT '',
    total_rows    UInt64  DEFAULT 0,
    sandbox_rows  UInt64  DEFAULT 0,
    position      UInt32  DEFAULT 0,
    column_name   String,
    type          String  DEFAULT '',
    class         Enum8('real' = 1, 'identifier' = 2, 'redacted' = 3, 'attrmap' = 4),
    usage         String  DEFAULT '',
    updated_at    DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY (anon_database, table_name, column_name);

CREATE VIEW IF NOT EXISTS bentoclick.schema_guide AS
SELECT run_id, anon_database, model, naming, table_name, table_role,
       total_rows, sandbox_rows, position, column_name, type, class, usage
FROM bentoclick.schema_guide_data FINAL;

ALTER TABLE bentoclick.schema_guide MODIFY COMMENT '{"title":"describe_schema","description":"START HERE. Lists every TOKENIZED MASKED SANDBOX you may query, one row per (anon_database, table, column). Filter by anon_database (the database you query, e.g. mydb_anon or system_anon). naming says whether table/column NAMES are tokens (tbl_<hex>/col_<hex>) or real. class is the contract per column: real = verbatim value, filter/group/aggregate freely; identifier = a deterministic token, use only for GROUP BY/JOIN/uniq (the literal is meaningless but it relabels to the real value for the human who views the dashboard); redacted = masked free text, never filter/group/show; attrmap = a Map whose keys are real and values are per-key roles (call describe_attributes). You query the sandbox with tokens/masked values; a saved dashboard is de-tokenized and the sandbox DB is rewritten to the real DB at view time, so the human sees real identifiers and data. The sandbox is a SAMPLE (sandbox_rows << total_rows): report ratios and shapes, not absolute totals."}';

CREATE TABLE IF NOT EXISTS bentoclick.attr_guide_data
(
    run_id        String,
    anon_database String,
    table_name    String,
    column_name   String,
    attr_key      String,
    role          Enum8('vocabulary' = 1, 'measure' = 2, 'identity' = 3, 'sensitive' = 4),
    usage         String  DEFAULT '',
    updated_at    DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY (anon_database, table_name, column_name, attr_key);

CREATE VIEW IF NOT EXISTS bentoclick.attr_guide AS
SELECT run_id, anon_database, table_name, column_name, attr_key, role, usage
FROM bentoclick.attr_guide_data FINAL;

ALTER TABLE bentoclick.attr_guide MODIFY COMMENT '{"title":"describe_attributes","description":"Per attribute KEY of an attrmap (Map) column: its role and how to use it. Filter by anon_database, table_name, column_name. vocabulary = real value (filter and group); measure = real number (aggregate); identity = masked, GROUP BY only (relabels to a real value for the human — use for per-entity breakdowns, never filter a masked literal); sensitive = masked free text (avoid). Keys are always real; values follow the role. Call describe_schema first."}';

-- LLM read role (live deployment uses anon_mcp_reader). The guide is the contract,
-- safe to expose; the de-anon secret lives in bentosecrets.* and is never granted here.
-- Plain views run with the invoker's rights, so grant the backing tables too.
GRANT SELECT ON bentoclick.schema_guide      TO anon_mcp_reader;
GRANT SELECT ON bentoclick.attr_guide        TO anon_mcp_reader;
GRANT SELECT ON bentoclick.schema_guide_data TO anon_mcp_reader;
GRANT SELECT ON bentoclick.attr_guide_data   TO anon_mcp_reader;
