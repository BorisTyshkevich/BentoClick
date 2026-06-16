---
name: bentoclick-dashboard
description: >
  Build dashboards backed by a ClickHouse cluster and save them so they
  can be shared by URL. Trigger when the user asks to build, visualize,
  or save a dashboard / chart / report.
---

# bentoclick-dashboard

> **Mandatory:** Before writing any panel JSON, read `panels/<type>.md` for
> every panel type you intend to use. Field names are not guessable from
> generic chart library conventions — `combo` uses `bars:{key}` + `line:{key}`,
> not `series:[…]`; `chart` uses `value_key`, not `series`. Reading the wrong
> field name produces a chart that renders axes and nothing else, with no error.

Dashboards are rows in `bentoclick.dashboards`, served at
`https://<spa-origin>` behind OAuth. The viewer's
bearer authenticates ClickHouse queries; the dashboard's SQL runs as
the viewer.

There is one dashboard shape: a **spec** — a small JSON object the SPA
renders to HTML at view time. Every spec is dynamic (panels re-fetch
on param change). No raw-HTML dashboards.

## Spec shape

```jsonc
{
  "title":        "string, required",  // <h1> + page <title>
  "subtitle":     "string, optional",  // muted line under H1
  "spec_version": 1,                   // pin to 1; bumps are breaking
  "concurrent":   false,               // panels fetch sequentially by default
  "params":       [ /* see Params */ ],
  "panels":       [ /* see Panels */ ]
}
```

## Save

Call the **`save_dashboard`** MCP tool with the spec values as
arguments. `slug` and `title` are required; the rest are optional and
map to the columns: `subtitle`, `concurrent`, `spec_version`, plus
`params`, `panels`, `meta`, `tags` — these last four are passed as
**JSON-encoded text strings** (the agent stringifies them; the server
parses on the way in).

The server writes through a sanitizing materialized view that strips
`<script>`, `<iframe>`, event handlers, and `javascript:` URLs from
`html` panels, then computes `owner = currentUser()` (the actual
caller; the MV uses `SQL SECURITY DEFINER` for privilege purposes
but `currentUser()` still resolves to the session user) and
`updated_at = now()`. A new INSERT under the same `(owner, slug)`
replaces the previous row.

## Share URL

Call the **`get_dashboards_prefix`** MCP tool once per conversation. It
returns `{owner, spa_origin, my_dashboards_prefix}`. Share URL =
`<my_dashboards_prefix><slug>`. Never guess the host — the MCP origin
and the SPA origin are different.

## Params

Render as `<input>` / `<select>` / date-picker above the panels. The
current value substitutes into each panel's query via `{{name}}`,
with strict per-type validation. Failure aborts the query and shows
inline in the panel — never sent to CH.

| `type`   | Validation | Splice form |
|---|---|---|
| `int`    | integer, optional `min` / `max` | literal: `123` |
| `enum`   | must be one of `options` | `'value'` (single-quoted) |
| `date`   | matches `^\d{4}-\d{2}-\d{2}$` | `'2024-01-15'` |
| `string` | matches `pattern` (default `^[A-Za-z0-9 _.+@-]*$`), `max_length` | `'value'` (quotes doubled) |

```jsonc
"params": [
  { "name": "year",    "type": "int",  "default": 2023, "min": 1987, "max": 2025 },
  { "name": "carrier", "type": "enum", "options": ["WN","DL","AA","UA"], "default": "WN" },
  { "name": "from",    "type": "date", "default": "2024-01-01" }
]
```

## Picking the dashboard shape

Decide the **shape** before picking panels. The shape determines
whether the period belongs in a param at the top of the page or
inside a chart axis.

- **Snapshot** — one period, many slices. Use a `param` for the
  period; panels slice that period (kpi-strip + tables + bars). The
  worked example below is this shape.
- **Time-series** — many periods, one or two metrics. Do **not** put
  a period param at the top; the time axis lives inside a `line` or
  `combo` panel. Params should constrain non-time dimensions
  (carrier, airport, region). Cross-period questions ("which year
  did X overtake Y") render on one screen.
- **Event log** — a small list of notable moments. Drive the page
  from one `table` panel; use `callouts` to narrate the rows; use
  `annotations` to mark the events on any time-series panel beside
  the table.

If the user's question spans periods, the answer is almost never
"add a year picker." Reach for `line`, `combo`, or `chart` with an
`annotations` source instead.

## Panels

Auto-flow layout: `width: 12` (default) is full-row; consecutive
`width: 6` pair side-by-side; consecutive `width: 4` triple. No
nested grids.

| `type` | Use for | Key fields (do NOT guess — read the panel doc) | Reference |
|---|---|---|---|
| `kpi-strip` | One-row query → tile per measurable | `tiles:[{key,label,format}]` | [panels/kpi-strip.md](panels/kpi-strip.md) |
| `table`     | Sticky-header table, formatters, threshold badges | `columns:[{key,label,format,align}]` | [panels/table.md](panels/table.md) |
| `bars`      | Horizontal share bars (quick shortcut) | `value_key`, `label_key` | [panels/bars.md](panels/bars.md) |
| `chart`     | Categorical bars with `color_by`, vertical or horizontal | `x_key`, `value_key`, `color_by`, `orientation` — **NOT** `series` | [panels/chart.md](panels/chart.md) |
| `line`      | Time-series / ordered x-axis with one or more series | read doc — has its own `series` shape | [panels/line.md](panels/line.md) |
| `combo`     | Bars + line on dual axes — analytical workhorse | `x_key`, `bars:{key,label,color_by}`, `line:{key,label,axis}` — **NOT** `series:[…]` | [panels/combo.md](panels/combo.md) |
| `markdown`  | Narrative text, no query | `content` | [panels/markdown.md](panels/markdown.md) |
| `hero`      | Templated sentence anchored to another panel's first row | `source`, `template` | [panels/hero.md](panels/hero.md) |
| `callouts`  | Templated cards anchored to N rows of another panel | `source`, `template` | [panels/callouts.md](panels/callouts.md) |
| `html`      | Static markup (sanitized) + optional templated `query` | `content` or `query`+`template` | [panels/html.md](panels/html.md) |
| `script`    | JS escape hatch — drill-down, third-party libs. Last resort. | read the two-of-three gate first | [panels/script.md](panels/script.md) |
| `dataset`   | Headless query; expose rows to other panels via `source` | `query`, `state:"hidden"` | [panels/dataset.md](panels/dataset.md) |

> **Rule:** before writing any panel, read its `panels/<type>.md`. The table above lists the most common wrong-field mistakes; it is not a substitute for the full spec.

`script` has a two-of-three decision gate at the top of its page.
Read it before reaching for `script`.

## Shared data and performance

**Rule — share, don't repeat.** If two or more panels would run the
same base table scan or CTE chain (≥ 1 shared `WITH` clause, or the
same `FROM … WHERE` against a fact table), extract that SQL into a
`dataset` panel declared first and have every consumer read from it
via `source` + `transform`. Default the dataset to `state: "hidden"`;
switch to `state: "collapsed"` only when the raw rows are useful for
debugging or presenting the underlying data. Never duplicate the SQL
across panels, and do not rely on ClickHouse's query cache to
deduplicate — small spec edits (different `LIMIT`, different
projection) silently bypass it.

**Trigger check before writing each panel's `query`:** does another
panel in this spec already start with the same `WITH …` block, or
scan the same fact table with the same `WHERE`? If yes — stop and
promote the shared SQL to a `dataset` first.

When several visual panels would otherwise repeat the same expensive
CTE chain, declare the query once as a `dataset` panel and consume it
from each visual panel:

- `source: "<panel-id>"` reads rows from another panel's result
  instead of issuing SQL. The source must be declared earlier in
  `panels` than the consumer.
- `transform: "<JS function body>"` optionally reshapes the rows for
  the consumer. Sync only. Receives `(rows, params)`; `rows` is a
  frozen deep copy, `params` is a frozen snapshot of the current
  param values. Must `return` an array.
- Setting both `query` and `source` is an error.
- `concurrent: true` still governs SQL execution. Source consumers
  always wait for their source's load to settle, regardless of mode.

For `kpi-strip` consumers, the transform should return a single-row
array (`[{ ...tiles }]`), since `kpi-strip` reads its tile values
from `rows[0]`.

## Panel state

Every panel accepts an optional `state` field:

| `state`     | Effect |
|---|---|
| `visible`   | Default for visual panels — full card. |
| `collapsed` | Card chrome (title) shows; body hidden via CSS. |
| `hidden`    | Card removed from layout entirely. Default for `dataset`. |

For `table` and `bars` panels, `state: "collapsed"` and the
`collapsible: true` field both refer to the **partial-collapse UX**
(first rows visible + gradient fade + "Show all rows" CTA), not the
chrome-only mode used for `dataset`. Long tables/bars auto-collapse
when their result has ≥ 50 rows; set `collapsible: false` to opt out.

When a `dataset` returns 50+ rows and is useful for debugging or
presenting the raw feed behind derived panels, declare it as
`state: "collapsed"` rather than leaving it visible — the title chrome
stays in layout so viewers can find it, but the bulky row dump stays
folded by default. Reserve `state: "visible"` for small datasets
(≤ ~20 rows) whose contents are immediately useful at a glance.

## Cross-panel filtering

Any `table`, `chart`, `combo`, or `line` panel can drive a param
update on click via `on_click`:

```jsonc
{
  "id": "handoffs", "type": "table",
  "columns": [ /* … */ ],
  "on_click": { "set_param": "year", "from": "year_of_change" }
}
```

Clicking a row (or a bar / point) writes `row[from]` to the named
param. Panels whose query interpolates `{{year}}` re-run; the rest
are untouched. The toolbar input syncs automatically.

`on_click` is the right answer when a click on a row should drill
into a snapshot of the corresponding period or entity. It is the
wrong answer for free-form filters — those are still params.

## Annotations

`line`, `combo`, and `chart` accept an `annotations` block that
overlays vertical reference lines from another panel's rows:

```jsonc
"annotations": {
  "source":    "handoffs",        // another panel id
  "x_key":     "year_of_change",  // matched against this chart's x_key
  "label_key": "new_leader"       // optional text label
}
```

Pairs naturally with the **event log** shape: a `table` lists the
moments, a `combo` plots the underlying metric, and the annotations
tie them together visually.

## Formatters

`format` values for tile/cell values:

| name | example |
|---|---|
| `raw` (default) | HTML-escape only |
| `num`        | `12,345` |
| `pct`        | `42.0%` (input is a fraction 0..1) |
| `cost`       | `$12.34` (`$12.345` below $10) |
| `date` / `day` | `YYYY-MM-DD` (first 10 chars) |
| `time`       | `13:05` (from hhmm integer like `1305`) |
| `duration`   | `2h 15m` (from minutes) |
| `bytes`      | `12.3 KB` |

`format_fn` references any `DASH.fmt.*` helper by dotted path —
e.g. `"format_fn": "fmt.shortEmail"` for the localpart of an email.

## spec_version

Always pin `spec_version: 1`. The SPA loads `/lib/v<N>/dash.js` to
render; old dashboards keep working when v2 ships.

## Don'ts

- Don't paste user input into SQL — declare a `param` and use `{{name}}`.
- Don't embed bearer tokens or `Authorization` headers in panels —
  `DASH.spec.fetch` handles auth.
- Don't reach for `localStorage`, cookies, or `parent.*` from `script`
  panels — the sandbox blocks them.
- Don't reach for `script` when a data panel can do it. Read
  [panels/script.md](panels/script.md) first.
- Don't set `concurrent: true` unless every panel query is cheap.
  Default sequential fetching keeps peak memory bounded.

## Worked example

```jsonc
{
  "title":        "US Airline On-Time",
  "spec_version": 1,
  "params": [{ "name": "year", "type": "int", "default": 2023, "min": 1987, "max": 2025 }],
  "panels": [
    {
      "id": "kpis", "type": "kpi-strip",
      "query": "SELECT count() AS flights, countIf(Cancelled=1) AS cancelled FROM ontime.fact_ontime WHERE Year = {{year}}",
      "tiles": [
        { "key": "flights",   "label": "Flights",   "format": "num" },
        { "key": "cancelled", "label": "Cancelled", "format": "num" }
      ]
    },
    {
      "id": "airlines", "type": "table", "width": 6, "title": "Top 10 airlines",
      "query": "SELECT Reporting_Airline AS airline, count() AS flights FROM ontime.fact_ontime WHERE Year = {{year}} GROUP BY airline ORDER BY flights DESC LIMIT 10",
      "columns": [
        { "key": "airline", "label": "Airline" },
        { "key": "flights", "label": "Flights", "format": "num", "align": "right" }
      ]
    },
    {
      "id": "airports", "type": "table", "width": 6, "title": "Top 10 origins",
      "query": "SELECT OriginCode AS code, count() AS flights FROM ontime.fact_ontime WHERE Year = {{year}} GROUP BY code ORDER BY flights DESC LIMIT 10",
      "columns": [
        { "key": "code",    "label": "Code" },
        { "key": "flights", "label": "Flights", "format": "num", "align": "right" }
      ]
    }
  ]
}
```
