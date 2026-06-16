-- otel anon dashboard pipeline — grants (apply via: cl otel --multiquery < grants.sql)
--
-- The de-tok materialized view runs as DEFINER bentoclick_definer and calls
-- detok() → dictGetOrDefault on the token→real dictionary, so the definer needs
-- dictGet on it (else save → 497 "Not enough privileges ... dictGet").
GRANT dictGet ON altinity.token_to_real TO bentoclick_definer;

-- The LLM channel activates only ^anon_ roles per request, so anon_author_role
-- must be able to read the share-URL view that backs the get_dashboards_prefix tool.
GRANT SELECT ON bentoclick.dashboards_prefix TO anon_author_role;

-- (schema_guide's grant to anon_mcp_reader lives in schema_guide.sql.)
