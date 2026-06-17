# `kpi-strip`

One query returning a single row; one tile per measurable. Each tile
is **value / label / optional italic note** — use the note for a
one-line interpretation ("Established schedule", "9 airports").

```jsonc
{
  "id": "kpis", "type": "kpi-strip", "width": 12,
  "query": "SELECT count() AS flights, uniqExact(Reporting_Airline) AS airlines, 'Most flown' AS flights_note FROM ontime.fact_ontime WHERE Year = {{year}}",
  "tiles": [
    { "key": "flights",  "label": "Flights",  "format": "num",
      "note_key": "flights_note" },                 // 3rd line from a column
    { "key": "airlines", "label": "Airlines", "format": "num",
      "note": "distinct carriers" }                 // 3rd line, literal string
  ]
}
```

## Tile fields

| Field | Purpose |
|---|---|
| `key`            | column from the row to display |
| `label`          | static label below the value |
| `format`         | `num`, `pct`, `cost`, `duration`, … — see SKILL.md |
| `format_fn`      | dotted path into `DASH.fmt.*` — overrides `format` |
| `note`           | literal string for the 3rd line |
| `note_key`       | column from the row for the 3rd line |
| `note_format`    | `format` for the note value |
| `note_format_fn` | `format_fn` for the note value |
| `accent`         | `primary` / `warn` / `error` — left-border color |

## Edges

- Missing measurable → renders as empty, no crash.
- Empty result set → tiles stay in skeleton state.
- Layout is auto-fit (uses CSS grid); never wraps awkwardly.
