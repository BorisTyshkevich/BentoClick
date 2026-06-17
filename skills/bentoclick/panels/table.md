# `table`

Sticky-header HTML table with per-column formatters, threshold
badges, and a CSV-export button. Rows render in declared order;
sort/filter UI isn't built in (do it in SQL).

```jsonc
{
  "id": "longest", "type": "table", "width": 6, "title": "Top 10 longest",
  "query": "SELECT tail, miles, air, hops FROM per_day ORDER BY miles DESC LIMIT 10",
  "columns": [
    { "key": "tail",  "label": "Tail" },
    { "key": "miles", "label": "Miles", "format": "num",      "align": "right" },
    { "key": "air",   "label": "Air",   "format": "duration", "align": "right" },
    { "key": "hops",  "label": "Hops",  "format": "num",      "align": "right",
      "badge": { "8+": "high", "7+": "mid", "0+": "low" } }
  ],
  "empty_text": "No rotations matching the filter",
  "export": true
}
```

## Column fields

| Field | Purpose |
|---|---|
| `key`       | column from each row |
| `label`     | header cell text |
| `format` / `format_fn` | cell formatter — see SKILL.md |
| `align`     | `"right"` for numeric columns |
| `badge`     | threshold → CSS class map (below) |

## `badge` thresholds → CSS class

Two equivalent forms:

```jsonc
// explicit, descending; first match wins. Use "0+" as a fallback.
"badge": { "20+": "high", "5+": "mid", "0+": "low" }

// compact; values are min thresholds for each named class.
"badge": { "high": 20, "mid": 5, "low": 0 }
```

Class names are prefixed with `cell-badge-` and styled in the dash
theme:

- `cell-badge-high` — accent green
- `cell-badge-mid`  — warn amber
- `cell-badge-low`  — error red

For other palettes, use a different class name and add CSS in an
`html` panel above the table (or in a `markdown` panel with raw
`<style>` — note that `<style>` *is* preserved by the sanitizer).

## Long result sets — `collapsible`

For tables that return many rows, the panel can render in a
partial-collapse mode: only the top of the table is visible behind a
soft gradient fade, with a full-width "Show all rows · N" button at
the bottom and a small chevron next to the title. Either trigger
expands the panel; expanded, the CSV / actions bar reappears.

| Value | Effect |
|---|---|
| `collapsible: true`  | Always install the collapse chrome; start collapsed unless `state: "visible"` is also set. |
| `collapsible: false` | Suppress the auto-trigger even on long result sets. |
| omitted              | Auto-enable when the result has ≥ 50 rows. |

`state: "collapsed"` on a table is the partial-collapse UX (title +
top rows + reveal CTA), *not* the chrome-only mode used by `dataset`
panels.

## Edges

- `rows = []` → renders `empty_text` (or "no data") spanning all columns.
- `export: false` hides the CSV button.
- `null` cells → coerced through the formatter (e.g. `num` → `0`).
- Column `align` only affects text alignment, not header alignment.
