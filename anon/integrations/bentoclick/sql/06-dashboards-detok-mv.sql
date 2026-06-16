DROP TABLE IF EXISTS bentoclick.dashboards_mv;

CREATE MATERIALIZED VIEW bentoclick.dashboards_mv TO bentoclick.dashboards
(
    `slug` String,
    `title` String,
    `subtitle` String,
    `concurrent` Bool,
    `spec_version` UInt8,
    `params` Array(JSON),
    `panels` Array(JSON),
    `meta` JSON,
    `tags` Array(String),
    `owner` String,
    `updated_at` DateTime
)
DEFINER = bentoclick_definer SQL SECURITY DEFINER
AS SELECT
    slug,
    title,
    subtitle,
    concurrent,
    spec_version,
    JSONExtract(params, 'Array(JSON)') AS params,
    JSONExtract(detok(replaceRegexpAll(replaceRegexpAll(panels, '(?is)<script\\b[^>]*>.*?<\\\\?/script[^>]*>|<iframe\\b[^>]*>.*?<\\\\?/iframe[^>]*>|<object\\b[^>]*>.*?<\\\\?/object[^>]*>|<svg\\b[^>]*>.*?<\\\\?/svg[^>]*>|<math\\b[^>]*>.*?<\\\\?/math[^>]*>|<style\\b[^>]*>.*?<\\\\?/style[^>]*>|<form\\b[^>]*>.*?<\\\\?/form[^>]*>|<(?:script|iframe|object|svg|math|style|form|embed|link|base|meta)\\b[^>]*\\\\?/?>|\\bon[a-z]+\\s*=\\s*(?:\\\\?"[^"]*\\\\?"|\'[^\']*\'|[^\\s>"\']+)', ''), '(?i)javascript:', '')), 'Array(JSON)') AS panels,
    CAST(meta, 'JSON') AS meta,
    JSONExtract(tags, 'Array(String)') AS tags,
    currentUser() AS owner,
    now() AS updated_at
FROM bentoclick.dashboards_raw;
