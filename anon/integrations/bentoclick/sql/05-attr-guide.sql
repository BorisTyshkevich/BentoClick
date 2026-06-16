-- altinity.attr_guide — backs the describe_attributes MCP tool.
-- Per attribute KEY of every Map column: its role + a usage hint, latest run.
-- Apply: cl otel --multiquery < attr_guide.sql
CREATE OR REPLACE VIEW altinity.attr_guide AS
SELECT
    table_token,
    col_token,
    attr_key,
    role,
    multiIf(
        role = 'vocabulary', 'real value: filter AND group by it',
        role = 'measure',    'real number: aggregate (sum/avg/count)',
        role = 'identity',   'masked: GROUP BY only (relabels to real for the human); never filter a literal; prefer over an opaque id',
        role = 'sensitive',  'masked high-cardinality free text: avoid',
        role
    ) AS usage
FROM altinity.profile_attr_keys
WHERE run_id = (SELECT max(run_id) FROM altinity.profile_attr_keys)
ORDER BY table_token, col_token, role, attr_key;

ALTER TABLE altinity.attr_guide MODIFY COMMENT '{"title":"describe_attributes","description":"Per attribute KEY of a Map column: its role and how to use it. vocabulary = real value (filter and group); measure = real number (aggregate); identity = masked, GROUP BY only — it relabels to a real value for the human, so use it for per-entity breakdowns (e.g. an email-like key), never an opaque id, and never filter on a masked literal; sensitive = masked free text (avoid). Keys are always real; values follow the role. Call describe_schema first for the table and column map."}';

-- INVOKER view → the LLM role needs SELECT on the view AND its underlying table.
GRANT SELECT ON altinity.attr_guide TO anon_mcp_reader;
GRANT SELECT ON altinity.profile_attr_keys TO anon_mcp_reader;
