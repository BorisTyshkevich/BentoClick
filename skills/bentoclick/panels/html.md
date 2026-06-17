# `html`

Static markup, optionally with a templated `query` whose first rows
substitute into the template. The MV sanitizes the markup at save
time (`<script>`, `<iframe>`, `on*=` handlers, `javascript:` URLs
are stripped), so the runtime trusts what it reads.

```jsonc
// Static — no query.
{
  "type": "html", "width": 12,
  "html": "<div class='note'>Snapshot taken at install time.</div>"
}
```

```jsonc
// Templated — substitutes after the query lands.
{
  "type": "html", "id": "lead", "width": 12,
  "query":    "SELECT max(amount) AS top FROM expenses",
  "template": "<div>Top expense: <strong>{{rows[0].top}}</strong></div>"
}
```

## Fields

| Field | Purpose |
|---|---|
| `html`     | static body (used when `query` is absent) |
| `query`    | optional CH query — populates `rows` for the template |
| `template` | markup with `{{rows[N].field}}` placeholders |
| `title`    | optional h2 |
| `accent`   | `primary` / `warn` / `error` |

## Template syntax

`{{rows[N].field}}` substitutes `rows[N][field]`, HTML-escaped.
Missing rows render empty.

## Edges

- `<script>` and JS execution are not allowed here. For JS use the
  `script` panel.
- `<style>` is preserved by the sanitizer (you can scope styles to
  the dashboard).
- The runtime injects content via `innerHTML`. Don't rely on it
  running scripts in any way — they're stripped server-side and
  CSP-blocked even if they slipped through.
