"""Spec-contract validation in the SECURITY DEFINER MV (issue #16).

The MV parses the agent-supplied JSON once into typed aliases and gates
the spec with ``throwIf(...)`` before a row is allowed into
``dashboards``. The compatibility rule is *a new MV accepts old specs;
an old MV declines specs newer than it knows* — so validation is
fail-closed on "newer than known": unknown/newer ``spec_version``,
unknown panel/param types, malformed params, and panels that set both
``query`` and ``source`` are rejected. Old/minimal specs always pass.

Accept cases assert real sample specs (the backward-compat surface) and
every known panel type land. Reject cases assert each gate fires (CH
error 395 / message prefixed ``bentoclick:``). All validation lives in
the MV — including slug/title non-emptiness — so there are no CHECK
constraints on the Null source table. A cross-check guard pins the MV's
accepted panel-type set to the runtime dispatcher and asserts the two MV
files share an identical validation body.
"""

from __future__ import annotations

import json
import pathlib
import re

import pytest

REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
SAMPLES_DIR = REPO_ROOT / "samples"
DASH_JS = REPO_ROOT / "runtime" / "v1" / "dash.js"
PLAIN_MV = REPO_ROOT / "schema" / "01-database.sql"
ANON_MV = REPO_ROOT / "anon" / "integrations" / "bentoclick" / "sql" / "03-dashboards-anon.sql"

KNOWN_PANEL_TYPES = [
    "kpi-strip", "table", "bars", "markdown", "hero", "callouts",
    "html", "script", "line", "combo", "chart", "dataset",
]
KNOWN_PARAM_TYPES = {"int", "enum", "date", "string"}


# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------

def _insert_spec(ch, slug, *, title="T", subtitle="", spec_version=1,
                 params=None, panels=None, meta=None, tags=None) -> None:
    """Insert a full spec via the SELECT path (JSON columns as text, the
    shape an agent / the MCP write tool produces)."""
    ch.command(
        f"INSERT INTO {ch.db_name}.dashboards_raw "
        "(slug, title, subtitle, spec_version, params, panels, meta, tags) "
        "SELECT %(slug)s, %(title)s, %(subtitle)s, %(sv)s, "
        "%(params)s, %(panels)s, %(meta)s, %(tags)s",
        parameters={
            "slug": slug, "title": title, "subtitle": subtitle, "sv": spec_version,
            "params": json.dumps(params if params is not None else []),
            "panels": json.dumps(panels if panels is not None else []),
            "meta": json.dumps(meta if meta is not None else {}),
            "tags": json.dumps(tags if tags is not None else []),
        },
    )


def _landed(ch, slug) -> int:
    return ch.query(
        f"SELECT count() FROM {ch.db_name}.dashboards FINAL WHERE slug = %(s)s",
        parameters={"s": slug},
    ).result_rows[0][0]


def _assert_gate_rejected(exc) -> None:
    """A throwIf gate aborts with CH 395 and a `bentoclick:`-prefixed msg."""
    msg = str(exc.value)
    assert "395" in msg or "bentoclick:" in msg, (
        f"expected an MV validation gate (throwIf) to fire; got: {msg}"
    )


def _runtime_panel_types() -> set[str]:
    """The panel-type keys the v1 runtime dispatches on (PANELS in dash.js)."""
    text = DASH_JS.read_text()
    block = text.split("export const PANELS")[1].split("};")[0]
    return set(re.findall(r"'([a-z0-9-]+)'\s*:\s*render", block))


def _mv_in_lists(sql_path) -> list[set[str]]:
    """Every ``'type') IN ( ... )`` literal set in an MV file (panels + params)."""
    text = sql_path.read_text()
    raw = re.findall(r"'type'\)\s*IN\s*\(([^)]*)\)", text)
    return [set(re.findall(r"'([a-z0-9-]+)'", chunk)) for chunk in raw]


def _mv_body(sql_path) -> str:
    """The dashboards_mv SELECT body — from ``SECURITY DEFINER AS`` through the
    statement-terminating ``;`` — with the DB name / TO target normalized so the
    plain (``${DB}``/``dashboards``) and anon (``bentoclick``/``dashboards_tok``)
    copies compare equal. The terminator is found by line (a ``;`` at line end),
    not by the first ``;`` — a throwIf message contains a literal ``;``."""
    lines = sql_path.read_text().splitlines()
    # anchor on the dashboards_mv CREATE, then the MV's own `... DEFINER AS` line
    anchor = next(i for i, ln in enumerate(lines) if "dashboards_mv" in ln)
    as_line = next(i for i in range(anchor, len(lines))
                   if "SECURITY DEFINER AS" in lines[i])
    out = []
    for ln in lines[as_line:]:
        out.append(ln)
        if ln.rstrip().endswith(";"):
            break
    body = "\n".join(out)
    # drop everything up to and including `... DEFINER AS` (the TO-target,
    # which differs plain vs anon, lives on the lines before it)
    body = body.split("SECURITY DEFINER AS", 1)[1]
    body = body.replace("${DB}", "DB").replace("bentoclick", "DB")
    body = body.replace("DB.dashboards_tok", "DB.dashboards")
    return body.strip()


# ---------------------------------------------------------------------------
# accept (backward compatibility)
# ---------------------------------------------------------------------------

SAMPLE_FILES = sorted(SAMPLES_DIR.glob("*.spec.json"))  # excludes samples/system/*


def test_sample_files_present():
    # Guard the loop below against silently testing nothing.
    assert SAMPLE_FILES, "expected sample spec files under samples/*.spec.json"


@pytest.mark.parametrize("sample", SAMPLE_FILES, ids=lambda p: p.stem)
def test_real_sample_specs_accept(ch, sample):
    spec = json.loads(sample.read_text())
    slug = sample.stem
    _insert_spec(
        ch, slug,
        title=spec.get("title", "T"),
        subtitle=spec.get("subtitle", ""),
        spec_version=spec.get("spec_version", 1),
        params=spec.get("params", []),
        panels=spec.get("panels", []),
        meta=spec.get("meta", {}),
        tags=spec.get("tags", []),
    )
    assert _landed(ch, slug) == 1, f"sample {slug} should land through the validating MV"


def test_minimal_spec_accepts(ch):
    # slug + title only — the smallest valid spec.
    _insert_spec(ch, "minimal")
    assert _landed(ch, "minimal") == 1


@pytest.mark.parametrize("ptype", KNOWN_PANEL_TYPES)
def test_each_known_panel_type_accepts(ch, ptype):
    _insert_spec(ch, f"t-{ptype}", panels=[{"type": ptype, "query": "SELECT 1"}])
    assert _landed(ch, f"t-{ptype}") == 1, f"panel type {ptype!r} should be accepted"


@pytest.mark.parametrize("ptype", sorted(KNOWN_PARAM_TYPES))
def test_each_known_param_type_accepts(ch, ptype):
    param = {"name": "p", "type": ptype}
    if ptype == "enum":
        param["options"] = ["a", "b"]
    _insert_spec(ch, f"p-{ptype}", params=[param])
    assert _landed(ch, f"p-{ptype}") == 1, f"param type {ptype!r} should be accepted"


# ---------------------------------------------------------------------------
# reject (decline unknown / newer / malformed)
# ---------------------------------------------------------------------------

def test_unknown_panel_type_rejected(ch):
    with pytest.raises(Exception) as exc:
        _insert_spec(ch, "bad", panels=[{"type": "definitely-not-a-panel"}])
    _assert_gate_rejected(exc)


def test_empty_panel_type_rejected(ch):
    with pytest.raises(Exception) as exc:
        _insert_spec(ch, "bad", panels=[{"type": "", "query": "SELECT 1"}])
    _assert_gate_rejected(exc)


def test_panel_with_both_query_and_source_rejected(ch):
    with pytest.raises(Exception) as exc:
        _insert_spec(ch, "bad", panels=[
            {"type": "table", "query": "SELECT 1", "source": "other"},
        ])
    _assert_gate_rejected(exc)


def test_panel_with_only_source_accepts(ch):
    # The gate rejects *both* query and source, not source alone (the
    # cross-panel reference shape used by real samples).
    _insert_spec(ch, "src-only", panels=[
        {"type": "table", "source": "upstream"},
    ])
    assert _landed(ch, "src-only") == 1


def test_param_unknown_type_rejected(ch):
    with pytest.raises(Exception) as exc:
        _insert_spec(ch, "bad", params=[{"name": "x", "type": "timestamp"}])
    _assert_gate_rejected(exc)


def test_param_missing_type_rejected(ch):
    with pytest.raises(Exception) as exc:
        _insert_spec(ch, "bad", params=[{"name": "x"}])
    _assert_gate_rejected(exc)


def test_param_missing_name_rejected(ch):
    with pytest.raises(Exception) as exc:
        _insert_spec(ch, "bad", params=[{"type": "int"}])
    _assert_gate_rejected(exc)


def test_enum_param_without_options_rejected(ch):
    with pytest.raises(Exception) as exc:
        _insert_spec(ch, "bad", params=[{"name": "e", "type": "enum"}])
    _assert_gate_rejected(exc)


def test_enum_param_with_empty_options_rejected(ch):
    with pytest.raises(Exception) as exc:
        _insert_spec(ch, "bad", params=[{"name": "e", "type": "enum", "options": []}])
    _assert_gate_rejected(exc)


def test_spec_version_two_rejected(ch):
    with pytest.raises(Exception) as exc:
        _insert_spec(ch, "bad", spec_version=2)
    _assert_gate_rejected(exc)


def test_spec_version_zero_rejected(ch):
    with pytest.raises(Exception) as exc:
        _insert_spec(ch, "bad", spec_version=0)
    _assert_gate_rejected(exc)


def test_empty_slug_rejected(ch):
    # slug/title non-emptiness is an MV throwIf gate (not a Null-table CHECK).
    with pytest.raises(Exception) as exc:
        _insert_spec(ch, "", title="T")
    _assert_gate_rejected(exc)


def test_empty_title_rejected(ch):
    with pytest.raises(Exception) as exc:
        _insert_spec(ch, "no-title", title="")
    _assert_gate_rejected(exc)


# ---------------------------------------------------------------------------
# drift guards: MV type sets vs runtime, and plain-vs-anon MV sync
# ---------------------------------------------------------------------------

def test_mv_panel_types_match_runtime_dispatcher():
    """The MV's accepted panel-type set must equal the runtime dispatcher's.
    Adding a renderer without widening the MV set (or vice versa) breaks
    here — see CLAUDE.md "How to add a panel type"."""
    runtime = _runtime_panel_types()
    assert runtime == set(KNOWN_PANEL_TYPES), (
        f"runtime dispatcher drifted from the known set:\n"
        f"  runtime: {sorted(runtime)}\n  known:   {sorted(KNOWN_PANEL_TYPES)}"
    )
    in_lists = _mv_in_lists(PLAIN_MV)
    panel_set = next((s for s in in_lists if "kpi-strip" in s), None)
    assert panel_set == runtime, (
        f"MV panel-type set drifted from the runtime dispatcher:\n"
        f"  MV:      {sorted(panel_set or [])}\n  runtime: {sorted(runtime)}"
    )


def test_mv_param_types_match_known_set():
    in_lists = _mv_in_lists(PLAIN_MV)
    param_set = next((s for s in in_lists if "int" in s), None)
    assert param_set == KNOWN_PARAM_TYPES, (
        f"MV param-type set drifted:\n  MV:    {sorted(param_set or [])}\n"
        f"  known: {sorted(KNOWN_PARAM_TYPES)}"
    )


def test_plain_and_anon_mv_validation_blocks_identical():
    """The plain MV (schema/01-database.sql) and the anon MV
    (03-dashboards-anon.sql) must share an identical SELECT body (inline
    parse + WHERE throwIf gates) modulo the DB name / TO target — tokens
    don't touch the validated fields."""
    plain = _mv_body(PLAIN_MV)
    anon = _mv_body(ANON_MV)
    assert "throwIf" in plain, "could not locate the MV validation body"
    assert plain == anon, (
        "plain and anon MV validation bodies diverged — keep them in sync"
    )
