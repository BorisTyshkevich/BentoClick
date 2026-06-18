CREATE OR REPLACE VIEW altinity.schema_guide AS
SELECT
    c.table_token AS table_token,
    c.role AS role,
    c.total_rows AS total_rows,
    c.sandbox_rows AS sandbox_rows,
    col.position AS position,
    col.col_token AS col_token,
    col.type_tok AS type,
    col.class AS class,
    multiIf(
        col.class = 'time',     'time axis / range filter; kept verbatim',
        col.class = 'measure',  'aggregate: sum/avg/count; kept verbatim',
        col.class = 'label',    'GROUP BY dimension; stable hashed token',
        col.class = 'joinkey',  'JOIN / uniq / high-card GROUP BY; deterministic hash',
        col.class = 'freetext', 'REDACTED; never group or filter on it',
        col.class = 'attrmap',  'Map: keys real, values per-key role — call describe_attributes for each key (filter/group/aggregate/avoid)',
        col.class
    ) AS usage
FROM altinity.profile_catalog AS c
INNER JOIN altinity.profile_columns AS col
    ON c.run_id = col.run_id AND c.db_token = col.db_token AND c.table_token = col.table_token
WHERE c.run_id = (SELECT max(run_id) FROM altinity.profile_catalog)
  AND c.sandboxed = 1
  AND c.sandbox_rows > 0
  AND col.included = 1
ORDER BY c.total_rows DESC, c.table_token, col.position;

ALTER TABLE altinity.schema_guide MODIFY COMMENT '{"title":"describe_schema","description":"START HERE before querying. This is an ANONYMIZED ClickHouse sandbox: table and column names are HMAC tokens (tbl_<hex>, col_<hex>) and values are masked. This tool returns the tokenized schema map: one row per (table, column) with the table role, the column class, and a usage hint. Query the masked data in mydb_anon.<table_token>. The class column is the contract: time and measure are kept verbatim (filter and aggregate); label and joinkey are deterministic hashes (GROUP BY, JOIN and uniq work, the literal value is meaningless); freetext is redacted (never group or filter on it); attrmap is a Map accessed by key (keys are real OTel semconv, values are masked). The sandbox is a SAMPLE (sandbox_rows is much smaller than total_rows): report ratios and shapes, not absolute totals."}';

GRANT SELECT ON altinity.schema_guide TO anon_mcp_reader;
