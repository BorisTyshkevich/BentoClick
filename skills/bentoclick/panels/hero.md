# `hero`

Templated narrative card pinned to another panel's first row. No
query of its own — it reads `panels[anchor].rows[0]` and re-renders
whenever that panel reloads.

Use it for "headline takeaway" prose that interprets the lead data
("On 2023-07-04 the **N12345** flew 9 hops…") rather than just
showing it.

```jsonc
{
  "id": "hero", "type": "hero", "title": "Lead Itinerary",
  "anchor": "longest",
  "template": "On **{{date!}}**, aircraft **{{tail!}}** flew {{hops|num}} legs covering {{miles|num}} miles in {{air|duration}}."
}
```

## Fields

| Field | Purpose |
|---|---|
| `anchor`   | id of another panel; hero reads its first row |
| `template` | the prose with `{{field}}` placeholders |
| `title`    | optional h2 |
| `accent`   | `primary` (default) / `warn` / `error` |

## Template syntax

| Marker | Meaning |
|---|---|
| `{{field}}`       | substitute `row[field]`, HTML-escaped |
| `{{field\|fmt}}`  | apply `DASH.fmt.<fmt>` (`num`, `duration`, `date`, …) |
| `{{field!}}`      | wrap value in `<span class="hl">` (accent color) |
| `{{field\|fmt!}}` | combine: formatted **and** highlighted |
| `**bold**` / `*italic*` / `` `code` `` / `[text](url)` | inline markdown |

## Edges

- Anchor panel hasn't loaded yet → renders "(waiting for `<anchor>`)"
  in muted text.
- Anchor row missing the field → renders an em-dash for that placeholder.
- Auto-re-renders when the anchor panel re-fetches (e.g. after a
  param change). No code needed.
