-- One-time backfill of bentoclick.schema_guide for the two existing sandboxes,
-- so the registry serves both without re-running the producers. Going forward
-- anond writes these rows directly. Safe to re-run (ReplacingMergeTree).

-- mydb_anon — tokenizing model, from anond's profile_* (latest run).
INSERT INTO bentoclick.schema_guide
  (run_id, anon_database, model, naming, table_name, table_role, total_rows, sandbox_rows, position, column_name, type, class, usage)
SELECT
    c.run_id,
    'mydb_anon', 'tokenizing', 'tokens',
    c.table_token, c.role, c.total_rows, c.sandbox_rows,
    col.position, col.col_token, col.type_tok,
    multiIf(col.class IN ('time','measure','enum'), 'real',
            col.class IN ('label','joinkey'),        'identifier',
            col.class = 'attrmap',                    'attrmap',
            'redacted'),
    multiIf(col.class='time','time axis / range filter',
            col.class='measure','aggregate: sum/avg/count',
            col.class='label','GROUP BY dimension (token relabels to real for the human)',
            col.class='joinkey','JOIN / uniq / high-card GROUP BY (deterministic token)',
            col.class='freetext','REDACTED; never group or filter',
            col.class='attrmap','Map: call describe_attributes per key',
            col.class)
FROM altinity.profile_catalog AS c
INNER JOIN altinity.profile_columns AS col
    ON c.run_id = col.run_id AND c.db_token = col.db_token AND c.table_token = col.table_token
WHERE c.run_id = (SELECT max(run_id) FROM altinity.profile_catalog)
  AND c.sandboxed = 1 AND c.sandbox_rows > 0 AND col.included = 1;

-- system_anon — schema-preserving model, from the system_anon.schema_guide VALUES view.
INSERT INTO bentoclick.schema_guide
  (run_id, anon_database, model, naming, table_name, table_role, total_rows, sandbox_rows, position, column_name, type, class, usage)
SELECT
    'sysanon', 'system_anon', 'schema-preserving', 'real',
    table, '', 0, 0, 0,
    column, type,
    multiIf(class = 'keep',   'real',
            class = 'redact', 'redacted',
            startsWith(class, 'tok:') OR startsWith(class, 'hash:'), 'identifier',
            'redacted'),
    multiIf(class = 'keep',   'verbatim value: filter / group / aggregate',
            class = 'redact', 'REDACTED; never group or filter',
            startsWith(class, 'tok:'), concat('identifier token (', substring(class, 5), '); GROUP BY only, relabels to real for the human'),
            startsWith(class, 'hash:'), 'deterministic id; GROUP BY / JOIN only, stays masked',
            class)
FROM system_anon.schema_guide;

-- attr_guide — attrmap key roles for mydb_anon (from anond's profile_attr_keys).
INSERT INTO bentoclick.attr_guide
  (run_id, anon_database, table_name, column_name, attr_key, role, usage)
SELECT
    run_id, 'mydb_anon', table_token, col_token, attr_key, role,
    multiIf(role = 'vocabulary', 'real value: filter and group',
            role = 'measure',    'real number: aggregate',
            role = 'identity',   'masked: GROUP BY only, relabels to real for the human',
            role = 'sensitive',  'masked free text: avoid',
            role)
FROM altinity.profile_attr_keys
WHERE run_id = (SELECT max(run_id) FROM altinity.profile_attr_keys);
