# `line`

> ⚠️ `line` is the **only** panel type that uses `series: [...]`.
> Do **not** copy this pattern to `combo` or `chart` — those use different field shapes entirely.

Time-series or any ordered x-axis. Multiple series via an explicit
`series` array (wide format) or `series_key` + `value_key` (pivoted
from long format).

```jsonc
{
  "id": "yearly_leaders", "type": "line",
  "query": "SELECT Year AS year, count() AS flights FROM ontime.fact_ontime GROUP BY year ORDER BY year",
  "x_key": "year",
  "series": [
    { "key": "flights", "label": "Total flights" }
  ]
}
```

Long-format pivot:

```jsonc
{
  "id": "by_carrier", "type": "line",
  "query": "SELECT Year AS year, Reporting_Airline AS k, count() AS v FROM ontime.fact_ontime GROUP BY year, k",
  "x_key": "year",
  "series_key": "k",
  "value_key":  "v"
}
```

## Fields

| Field | Purpose |
|---|---|
| `query`       | rows in x-order |
| `x_key`       | column for the x-axis value |
| `series`      | array of `{ key, label, color? }` — wide format |
| `series_key`  | column whose distinct values become series labels — long format |
| `value_key`   | column for the numeric y value when using `series_key` |
| `x_format`    | formatter for x tick labels (default `"raw"`) |
| `y_format`    | formatter for y tick labels (default `"num"`) |
| `annotations` | see [SKILL.md → Annotations](../SKILL.md#annotations) |
| `on_click`    | see [SKILL.md → Cross-panel filtering](../SKILL.md#cross-panel-filtering) |
| `accent`      | left-border accent (`primary` / `secondary` / `warm` / `rose`) |
| `title`, `subtitle`, `empty_text` | `subtitle` renders as the `.ph-sub` line under the title |

## Interactivity (free, no spec needed)

- **Auto-stamp** in the panel header — shows the x-range (e.g.
  `1987 – 2025`) plus the query's elapsed time from the ledger
  (`· 122 ms`).
- **Hover crosshair + tooltip** — mouse over the chart snaps a dashed
  vertical line to the nearest x; the tooltip lists the x label and
  each series' formatted value.
- **Click-toggle legend** — when multiple series render, clicking a
  legend item fades it (`.item.off`) and hides that series' path +
  dots. Click again to restore.

## Edges

- The y-axis is clamped to include `0` so growth-from-zero series
  read accurately. Override by widening the data range.
- With > 12 x-values the renderer decimates x-axis tick labels to
  ~8 visible labels. The path itself still draws all points.
- Non-finite values are skipped in the path (no NaN segments).
