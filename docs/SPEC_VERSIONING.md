# spec_version — runtime contract versioning

Every dashboard row carries a `spec_version` (UInt8, default 1). The
SPA reads it and loads `/lib/v<spec_version>/dash.js` to render. This
lets old dashboards keep working forever while new dashboards opt
into newer runtime contracts.

## The contract

`spec_version = N` means **"this dashboard's `panels` and `params`
match what the v<N> runtime expects."** Concretely, the v1 contract
guarantees:

- Panel types: `kpi-strip`, `table`, `bars`, `markdown`, `hero`,
  `callouts`, `html`, `script`, `line`, `combo`, `chart`, `dataset`.
  Unknown types render an error tile in the runtime and are rejected
  at write time by the MV (see "Validation at the write layer" below).
- Param types: `int`, `enum`, `date`, `string`. Strict per-type
  validation at substitution time.
- `{{name}}` substitution everywhere SQL is templated.
- Sanitization at the MV layer strips `<script>`, `<iframe>`,
  `<object>`, `<embed>`, `on*=` event handlers, `javascript:` URLs
  from `html` panels.
- `script` panels execute as the viewer with no ACL gating
  (open-to-all-viewers v1 trust model).

## How the runtime is selected

The SPA's `synthesizeSpecWrapper` (in `runtime/v1/spa.js`) reads the
row's `spec_version` and emits:

```html
<script type="module">
  import { renderSpec } from "/lib/v<N>/dash.js";
  renderSpec(<spec>, document.getElementById("dash-root"));
</script>
```

For v1, this points at `/lib/v1/dash.js`. For v2 (when it lands),
new dashboards saved with `spec_version: 2` will load
`/lib/v2/dash.js`. Old v1 dashboards still load v1.

## When to bump

Bump the contract version when:

- A panel-type's shape changes incompatibly (renaming/removing a
  field, changing what a value means).
- A panel-type is removed.
- A spec-level field changes meaning.
- Sanitization rules tighten in a way that would silently break old
  HTML panels.

Don't bump for:

- Adding a new panel type that nothing else relies on.
- Adding a new optional field to an existing panel type.
- Adding a new formatter.
- Bug fixes to renderers that don't change the contract.

Most additive changes can land in v1 without a version bump.

## Writer requirements

The reflected MCP write tool exposes `spec_version` as a typed
parameter (UInt8, 1–255). Agents should pin it explicitly:
`spec_version: 1` for the current contract. Future agents that know
about v2 can pin `spec_version: 2`.

## Validation at the write layer

The `dashboards_mv` (`SQL SECURITY DEFINER`) validates the spec
contract as it parses the JSON, before a row is allowed into
`dashboards`. This is **fail-closed on "newer than known"** — the
compatibility rule is *a new MV accepts old specs; an old MV declines
specs newer than it knows*:

- **`spec_version`** is gated as a ceiling, not pinned: `0` and any
  value `> MAX_KNOWN` (currently `1`) are rejected. Old/minimal specs
  in `[1..MAX_KNOWN]` keep writing; a future v2 only needs the ceiling
  widened plus a migration. *Declines newer.*
- **Panel types** outside the 12 known types (above) are rejected, and
  no panel may set both `query` and `source`. *Declines unknown.*
- **Param types** outside `{int, enum, date, string}` are rejected;
  every param needs a non-empty `name`, and an `enum` param needs a
  non-empty `options` array. *Declines unknown.*
- **`slug`/`title` non-empty** is gated in the MV too. (All validation
  lives in the MV — there are no CHECK constraints on the Null source
  table, since `ALTER … ADD CONSTRAINT` is unsupported on `Null` and so
  couldn't be carried onto an existing cluster by a migration.)

A rejected INSERT aborts with CH error `395`
(`FUNCTION_THROW_IF_VALUE_IS_NON_ZERO`, message prefixed `bentoclick:`).
The DB layer is therefore **not** permissive: it rejects anything
outside the known contract rather than passing it through. The gates are
inline `throwIf(...)` in the MV's `WHERE`, parsing each JSON column once
in the `SELECT` (no `WITH` block — re-aliasing a parsed column back to
its source name trips `CYCLIC_ALIASES` on ClickHouse's old analyzer, and
the MV must work whether the inserter's session uses the old or the new
analyzer). The
known type sets are written inline in **both** MVs (`schema/01-database.sql`
and `anon/integrations/bentoclick/sql/03-dashboards-anon.sql`) and must
be kept in sync — a cross-check test in
`tests/schema/test_spec_validation.py` guards the drift. Existing
clusters pick up the gates via `ALTER TABLE … MODIFY QUERY` migrations
(`schema/migrations/`); `install.sh` no-ops an already-created MV. MCP
tool validation remains a useful first line of defense for friendlier
error messages, but the MV is the enforced bouncer.

## Runtime refuses unknown future versions

If a dashboard pins `spec_version: 7` and the SPA only has
`/lib/v1/` and `/lib/v2/`, the `import { renderSpec } from
"/lib/v7/dash.js"` fails (404). The wrapper script catches the
import error and renders an "unknown spec version" message rather
than a blank page.

The reverse is fine: a v2 runtime can render a v1 dashboard if it
keeps the v1 contract internally — but the *cleanest* policy is to
never let v2+ runtimes render v1 dashboards. They route via
`/lib/v1/dash.js` instead.

## File layout

```
runtime/
└── v1/
    ├── dash.js              # v1 runtime
    ├── dash-theme.css
    ├── spa.js               # SPA shell (version-agnostic for now)
    ├── spa.html
    └── oauth-callback.html
```

When v2 lands, `runtime/v2/` sits alongside `runtime/v1/`. Both are
served under `/lib/v1/` and `/lib/v2/` by the install script. SPA
shell may or may not version-bump separately.
