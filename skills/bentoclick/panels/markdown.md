# `markdown`

Narrative text, no query. The `text` field renders with a minimal
markdown subset: headings (`#`, `##`, `###`), **bold**, *italic*,
`code`, [links](https://…), and unordered (`- `) lists.

```jsonc
{
  "type": "markdown",
  "text": "Snapshot of **2023** activity. Source: [BTS](https://www.transtats.bts.gov/)."
}
```

## Fields

| Field | Purpose |
|---|---|
| `text`   | the markdown body (required) |
| `title`  | optional h2 above the body |
| `accent` | `primary` / `warn` / `error` — left-border color |

## Edges

- Everything not in the subset above is escaped — `<script>` etc.
  becomes plain text.
- Links require an `http://` or `https://` prefix; bare URLs and
  `javascript:` protocols are not linked.
- No tables, no images, no raw HTML passthrough. For raw HTML use
  the `html` panel.
