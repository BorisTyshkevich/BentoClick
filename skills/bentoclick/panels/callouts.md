# `callouts`

Templated cards anchored to **N rows** of another panel's result.
Sibling to [`hero`](hero.md), which is single-row only. Same
`{{key|fmt!}}` grammar.

```jsonc
{
  "id": "handoff_callouts", "type": "callouts",
  "anchor":   "handoffs",
  "rows":     "all",
  "template": "**{{year_of_change}}** — {{old_leader}} → {{new_leader!}}: margin flipped from {{margin_prev|num}} to {{margin_this|num}}."
}
```

## Fields

| Field | Purpose |
|---|---|
| `anchor`    | id of the panel whose rows drive the callouts |
| `rows`      | `"all"` (default), `N` (head limit), or `[0, 2, …]` (indices) |
| `template`  | per-row template — same grammar as `hero` |
| `accent`    | left-border accent (default `"primary"`) |
| `title`     | optional h2 above the list |

## Template grammar

- `{{key}}` — raw cell, HTML-escaped.
- `{{key|fmt}}` — runs through a formatter (`num`, `pct`, `cost`,
  `date`, `day`, `time`, `duration`, `bytes`, etc).
- `{{key!}}` — wraps the value in `<span class="hl">…</span>` for
  accent-colored emphasis.
- Inline markdown: `**bold**`, `*italic*`, `` `code` ``,
  `[text](https://url)`.
- Missing keys render as `—`, not `undefined`.

## Edges

- The anchor panel must run a query — `callouts` reads
  `spec.panels[anchor].rows`. Static panels (`markdown`, plain
  `html`) won't work as anchors.
- Re-renders automatically when the anchor's `panel:loaded` event
  fires (after a param change, after a click filter).
- Pairs naturally with the **event log** dashboard shape: one
  driver `table`, one `callouts` narrating each event, one
  `combo`/`line` with `annotations` showing them on the time axis.
