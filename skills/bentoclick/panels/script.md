# `script` panels — the JS escape hatch

`script` is a panel type, not a separate dashboard mode. Use it when a
spec panel genuinely can't express what's needed. Sandboxed iframe,
owner-authored JS, viewer-executed.

## Decision gate (read before writing one)

Before reaching for `script`, confirm **at least two** of these are true:

1. The data can't come from a panel `query` — it depends on another
   panel's rows, a click selection, or a value computed from siblings.
2. The interaction can't be a `params` toolbar input — toolbar params
   re-run all panels uniformly; you need targeted updates or DOM events.
3. The formatting can't be a `format_fn` / `badge` map / column
   formatter on an existing panel.

One failure is not enough. A KPI that needs unusual formatting is a
`format_fn`, not a `script` panel. A table that needs an extra column
is a SQL change, not a `script` panel.

If two or more fail, `script` is the right tool.

## Schema

```jsonc
{
  "type": "script",
  "id": "drill",           // required; unique within the spec
  "width": 12,             // optional; auto-flow grid (default 12)
  "title": "Details",      // optional; rendered above the panel shell
  "html": "<div id='drill-root'>Click a row above…</div>",
  "script": "await DASH.spec.ready; /* … */"
}
```

- `html` is the DOM shell rendered into the panel before `script` runs.
  No `<script>` tags allowed here — they're stripped at save time and
  again at render time. For JS, use the `script` field.
- `script` is the JS body executed once, after the panel's DOM is
  inserted and `DASH.spec.ready` has resolved.

## Lifecycle

```
spec parsed
  ↓
data panels query in declared order
  ↓
each panel renders into its slot — DASH.spec.panels.<id> populated
  ↓
DASH.spec.ready resolves
  ↓
script panels execute in declared order
  ↓
panel:loaded events fire (including for re-runs on param change)
```

The `script` body runs **once** at first render. Re-runs on param
change re-execute the upstream data panels but do **not** re-execute
the `script` body. To react to upstream re-runs, register a
`panel:loaded` listener inside the script.

## Runtime API

Available to every `script` panel:

| Need | API |
|---|---|
| Wait for initial render to complete | `await DASH.spec.ready` |
| Read another panel's rows | `DASH.spec.panels.<id>.rows` |
| Read another panel's `<tbody>` (tables only) | `DASH.spec.panels.<id>.tbodyEl` |
| Read another panel's root element | `DASH.spec.panels.<id>.el` |
| Read current param values | `DASH.spec.params` (live snapshot) |
| React to a panel re-running | `DASH.spec.on('panel:loaded', fn)` |
| React to any param change | `DASH.spec.on('params', fn)` |
| Apply `{{name}}` substitution | `DASH.spec.interpolate(sqlTemplate)` |
| Fire a labeled, ledger-tracked query | `await DASH.spec.fetch(label, sql)` |
| Format a value the same way columns do | `DASH.fmt.num(x)`, `DASH.fmt.duration(x)`, etc. |

`DASH.spec.fetch` returns `{ rows, columns, elapsed_ms }`. Errors
surface in the panel's own slot, not as `throw`s — listen on the
returned promise's rejection if you need custom handling.

## Pattern A — cross-panel display

A `script` panel that reads a KPI panel's first row and writes a
narrative line beneath the data.

```jsonc
{
  "type": "script", "id": "summary", "width": 12,
  "html": "<div class='card muted'>Peak this window: <span id='peak'>—</span>.</div>",
  "script": "await DASH.spec.ready;\nfunction render() {\n  const r = DASH.spec.panels.kpis.rows?.[0];\n  if (r) document.getElementById('peak').textContent = r.max_hops + ' hops';\n}\nrender();\nDASH.spec.on('panel:loaded', e => { if (e.id === 'kpis') render(); });"
}
```

## Pattern B — click-to-drill

A `script` panel that makes another panel's table rows clickable and
fetches a detail query when the user picks one.

```jsonc
{
  "type": "script", "id": "drill", "width": 12,
  "html": "<div class='card'><h2 id='dt'>Click a row above</h2><div id='db' class='muted'>—</div></div>",
  "script": "await DASH.spec.ready;\n\nfunction bind() {\n  const st = DASH.spec.panels.longest;\n  if (!st || !st.tbodyEl) return;\n  st.tbodyEl.querySelectorAll('tr').forEach((tr, i) => {\n    tr.classList.add('click');\n    tr.addEventListener('click', () => show(st.rows[i]));\n  });\n}\nbind();\nDASH.spec.on('panel:loaded', e => { if (e.id === 'longest') bind(); });\n\nasync function show(row) {\n  document.getElementById('dt').textContent =\n    `${row.tail} on ${row.date} (${row.hops} legs)`;\n  document.getElementById('db').textContent = 'Loading legs…';\n  const r = await DASH.spec.fetch('legs',\n    `SELECT CRSDepTime, OriginCode, DestCode, Distance\n       FROM ontime.fact_ontime\n      WHERE FlightDate  = '${row.date}'\n        AND Tail_Number = '${row.tail}'\n      ORDER BY CRSDepTime`);\n  document.getElementById('db').innerHTML = r.rows.map(l => {\n    const t = String(l.CRSDepTime).padStart(4,'0');\n    return `${t.slice(0,2)}:${t.slice(2)} ${l.OriginCode} → ${l.DestCode} (${l.Distance} mi)`;\n  }).join('<br>');\n}"
}
```

## Pattern C — third-party widget

A `script` panel that hosts a JS library not exposed by spec panels
(network graph, geo map, custom chart). The library must be either
already bundled into the runtime or loaded from a URL allowed by the
sandbox's CSP.

```jsonc
{
  "type": "script", "id": "chart", "width": 12,
  "html": "<div class='card'><div id='canvas' style='height:320px'></div></div>",
  "script": "await DASH.spec.ready;\nconst rows = DASH.spec.panels.monthly.rows;\n// renderWith(window.SomeChartLib, rows);"
}
```

If the library is not already bundled, ask the user before adding a
new CDN to the allow-list — the dashboard CSP is shared across all
dashboards on this install.

## Pitfalls

### SQL splicing from JS — only CH-trusted values

Values read from a sibling panel's `rows` came from a CH query and are
safe to splice into another query string. Values from anywhere else
(URL hash, an `<input>` the script added, `window.name`, etc.) are
**not** safe and must be validated by shape before splicing.

The cleanest way is to put the value through the panel's `params` and
use `DASH.spec.interpolate('… {{name}}')` — that runs the same strict
per-type validation as toolbar inputs.

```js
// SAFE: row.date is a CH date string from a panel query
const r = await DASH.spec.fetch('x', `… WHERE FlightDate = '${row.date}'`);

// SAFE: route an arbitrary value through validated params
DASH.spec.params.tail = userInput;             // throws if invalid
const sql = DASH.spec.interpolate('… WHERE Tail_Number = {{tail}}');
const r   = await DASH.spec.fetch('x', sql);

// UNSAFE: never do this
const r = await DASH.spec.fetch('x', `… WHERE Tail_Number = '${urlHash}'`);
```

### Don't block the runtime

`script` panels share the iframe. A `while(true)`, a synchronous
expensive computation, or an unhandled `await` that never resolves
will freeze the rest of the dashboard. Use `setTimeout(..., 0)` to
yield if you have heavy work; prefer chunked rendering for large
result sets.

The runtime wraps each `script` body in try/catch and routes errors
to the panel's own slot — one bad script panel does not break
siblings, but it does not auto-recover either.

### No `parent.*`, no `localStorage`, no cookies

The sandbox blocks them. Don't waste tokens trying. State that needs
to persist across reloads must live in CH (a saved dashboard row or
a user-owned table), not in the iframe.

### Visible to every viewer

In the current trust model, any authenticated viewer who can load
`/v/<owner>/<slug>` executes the dashboard's `script` panels. Do not
put owner-only logic, secrets, or trust-checks in `script` — the JS
source is visible to viewers (view-source on the rendered HTML).
A future spec_version will gate `script` execution on a per-row
viewer ACL; until then, treat `script` as public.

### Re-runs do not re-execute `script`

The script body runs once. Param changes trigger `panel:loaded` on
data panels — register listeners inside the script to react. Don't
assume top-level code re-runs.

```js
// WRONG — runs once, will not update when `year` changes
const yr = DASH.spec.params.year;
document.getElementById('hdr').textContent = `Year: ${yr}`;

// RIGHT — listener fires on every param change
function render() {
  document.getElementById('hdr').textContent = `Year: ${DASH.spec.params.year}`;
}
render();
DASH.spec.on('params', render);
```

## When to drop back to a regular panel

If the `script` body is more than ~30 lines, ask whether one of these
covers the case instead:

- A new `kpi-strip` tile with a `format_fn` for unusual formatting.
- A `table` column with a `badge` map for threshold coloring.
- An additional `params` entry to drive the query directly.
- A `hero` panel for templated narrative pinned to a panel's first row.

If none fit, `script` is correct. Keep the body minimal and document
the gate it passed at the top as a one-line comment so future readers
know why JS was unavoidable.
