# `chart`

> ❌ Do **not** use `series: [...]` or `series_key` — those belong to `line`.
> Use `x_key` + `value_key`. Using `series` produces axes with no data rendered and no error message.

Categorical bars with `color_by`, optional horizontal layout, and
`on_click` cross-panel filtering. Use this when `bars` runs out —
specifically when you need per-row color from a column, an actual
axis, or annotations.

```jsonc
{
  "id": "yearly_leaders", "type": "chart",
  "query": "SELECT Year AS year, leader_airline, leader_flights FROM yearly_leaders ORDER BY year",
  "x_key": "year",
  "value_key": "leader_flights",
  "color_by":  "leader_airline",
  "on_click":  { "set_param": "year", "from": "year" }
}
```

## Fields

| Field | Purpose |
|---|---|
| `query`       | one row per bar |
| `x_key`       | column for the x label (also `label_key`) |
| `value_key`   | column for the numeric value |
| `color_by`    | optional column; same value → same color across renders |
| `orientation` | `"vertical"` (default) or `"horizontal"` |
| `format`      | formatter for y-axis labels (default `"num"`) |
| `value_label` | tooltip label for the value series (vertical mode); defaults to `value_key` |
| `annotations` | see [SKILL.md → Annotations](../SKILL.md#annotations) |
| `on_click`    | see [SKILL.md → Cross-panel filtering](../SKILL.md#cross-panel-filtering) |
| `accent`, `title`, `subtitle`, `empty_text` | `subtitle` renders below the title in `.ph-sub` |

## Layout differences by orientation

| `orientation` | DOM | Hover | Best for |
|---|---|---|---|
| `vertical` (default) | SVG with axes, grid, vertical bars | crosshair + tooltip | time-series / wide categorical x |
| `horizontal` | HTML `.bc-bars > .row-b > .lbl + .track + .fill + .val` | CSS `:hover` on each row | top-N rankings with long row labels |

Horizontal mode shares its DOM with the `bars` panel and picks up the
same 8px-track styling. Vertical mode shares the SVG hover/legend
behavior with `line` and `combo`.

## Interactivity (vertical, free)

- Auto-stamp in header: row count + ledger elapsed_ms.
- Hover crosshair + tooltip with the value-series label + formatted
  value at the nearest x band.

## Edges

- Vertical mode caps visible x-axis tick labels at ~8 to keep them
  readable; the bars themselves still draw for every row.
- Bars without `color_by` use a single palette color. The shorthand
  for "all the same color" is to omit `color_by`.
