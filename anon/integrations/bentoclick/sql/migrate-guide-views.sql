-- One-time migration: convert bentoclick.schema_guide / attr_guide from TABLES
-- to VIEWS over backing *_data tables (data preserved via RENAME). The MCP
-- discovers read tools via `system.tables WHERE engine='View'`, so the guide
-- objects must be VIEWS. Idempotent-ish: run once after the registry tables exist.

RENAME TABLE bentoclick.schema_guide TO bentoclick.schema_guide_data;
RENAME TABLE bentoclick.attr_guide   TO bentoclick.attr_guide_data;

CREATE VIEW bentoclick.schema_guide AS
SELECT run_id, anon_database, model, naming, table_name, table_role,
       total_rows, sandbox_rows, position, column_name, type, class, usage
FROM bentoclick.schema_guide_data FINAL;
ALTER TABLE bentoclick.schema_guide MODIFY COMMENT '{"title":"describe_schema","description":"START HERE. Lists every TOKENIZED MASKED SANDBOX you may query, one row per (anon_database, table, column). Filter by anon_database (the database you query, e.g. claude_otel_anon or system_anon). naming says whether table/column NAMES are tokens (tbl_<hex>/col_<hex>) or real. class is the contract per column: real = verbatim value, filter/group/aggregate freely; identifier = a deterministic token, use only for GROUP BY/JOIN/uniq (the literal is meaningless but it relabels to the real value for the human who views the dashboard); redacted = masked free text, never filter/group/show; attrmap = a Map whose keys are real and values are per-key roles (call describe_attributes). You query the sandbox with tokens/masked values; a saved dashboard is de-tokenized and the sandbox DB is rewritten to the real DB at view time, so the human sees real identifiers and data. The sandbox is a SAMPLE (sandbox_rows << total_rows): report ratios and shapes, not absolute totals."}';

CREATE VIEW bentoclick.attr_guide AS
SELECT run_id, anon_database, table_name, column_name, attr_key, role, usage
FROM bentoclick.attr_guide_data FINAL;
ALTER TABLE bentoclick.attr_guide MODIFY COMMENT '{"title":"describe_attributes","description":"Per attribute KEY of an attrmap (Map) column: its role and how to use it. Filter by anon_database, table_name, column_name. vocabulary = real value (filter and group); measure = real number (aggregate); identity = masked, GROUP BY only (relabels to a real value for the human — use for per-entity breakdowns, never filter a masked literal); sensitive = masked free text (avoid). Keys are always real; values follow the role. Call describe_schema first."}';

GRANT SELECT ON bentoclick.schema_guide      TO anon_mcp_reader;
GRANT SELECT ON bentoclick.attr_guide        TO anon_mcp_reader;
GRANT SELECT ON bentoclick.schema_guide_data TO anon_mcp_reader;
GRANT SELECT ON bentoclick.attr_guide_data   TO anon_mcp_reader;
