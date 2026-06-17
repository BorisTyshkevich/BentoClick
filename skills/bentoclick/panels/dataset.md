# `dataset`

Headless query whose rows feed other panels via `source`. Use this
when several visual panels would otherwise repeat the same expensive
CTE chain — define the chain once, fan out to consumers.

**Rule.** If two or more panels share a base scan or CTE chain
(same `WITH …` block, or same `FROM … WHERE` against a fact table),
promote that SQL to a `dataset` declared first and consume from it
via `source`. Default `state: "hidden"`; use `state: "collapsed"`
only when the raw rows are useful for debugging or presenting the
underlying data. Do not duplicate SQL across panels.

```jsonc
{
  "id":    "scored_combinations",
  "type":  "dataset",
  "title": "Scored carrier/route combinations",
  "query": "WITH base AS (...) SELECT carrier, route, late_flights, avg_delay FROM scored ORDER BY late_flights DESC LIMIT 200"
}
```

A consumer reads from it:

```jsonc
{
  "id":     "worst_routes",
  "type":   "table",
  "source": "scored_combinations",
  "transform": "return rows.slice(0, 15);",
  "columns": [
    { "key": "carrier", "label": "Carrier" },
    { "key": "route",   "label": "Route" }
  ]
}
```

## Fields

| Field   | Purpose |
|---|---|
| `query` | the SQL, required |
| `id`    | required — consumers reference this via `source` |
| `title` | optional — only displayed when `state` is `visible` or `collapsed` |
| `state` | `hidden` (default), `collapsed`, `visible` |

When `state` is `collapsed` or `visible`, the panel renders a preview
table of its rows (column headers derived from the first row's keys)
with the same partial-collapse chrome as the `table` panel: a chevron
in the header and a "Show all rows · N" / "Collapse" CTA at the
bottom. `collapsed` starts folded; `visible` starts expanded; both
expose the toggle. `hidden` keeps the panel completely out of layout.

## Rules

- `source` and `query` are mutually exclusive on the consumer.
- The source must appear earlier in `panels` than the consumer
  (forward references error at boot).
- `transform` runs in a sandboxed scope with bindings `(rows, params)`
  only — no `DASH`, no fetch, no other-panel access. Sync only; must
  return an array.
- `rows` passed to `transform` is a frozen deep copy. Mutating a row
  throws.
- For `kpi-strip` consumers, return a single-row array
  (`return [{ tile1: ..., tile2: ... }];`).
- In `concurrent: true` specs, source consumers still serialize behind
  their source's load — only SQL-fetching panels race.
- A param change re-runs the dataset and walks forward to re-run every
  consumer transitively.
