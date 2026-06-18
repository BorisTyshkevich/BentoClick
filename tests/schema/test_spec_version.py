"""spec_version: default, range, and rejection behavior.

The runtime gates renderable versions in JavaScript (the SPA loads
/lib/v<N>/dash.js). As of issue #16 the DB layer is no longer permissive:
the SECURITY DEFINER MV declines a spec_version newer than it knows
(``0`` or ``> MAX_KNOWN``, currently 1) so an old MV refuses to store a
spec it can't faithfully serve. Backward compat is preserved — old/minimal
specs in ``[1..MAX_KNOWN]`` keep writing. The comprehensive accept/reject
matrix lives in ``test_spec_validation.py``; here we cover the
spec_version column specifics (default, coercion, type).
"""

from __future__ import annotations

import pytest


def _insert(ch, slug: str, *, spec_version=None) -> None:
    cols = ["slug", "title"]
    select_parts = ["%(slug)s", "%(title)s"]
    params: dict = {"slug": slug, "title": slug}
    if spec_version is not None:
        cols.append("spec_version")
        select_parts.append("%(sv)s")
        params["sv"] = spec_version
    ch.command(
        f"INSERT INTO {ch.db_name}.dashboards_raw ({', '.join(cols)}) "
        f"SELECT {', '.join(select_parts)}",
        parameters=params,
    )


def _read_spec_version(ch, slug: str) -> int:
    return ch.query(
        f"SELECT spec_version FROM {ch.db_name}.dashboards FINAL "
        "WHERE slug = %(slug)s",
        parameters={"slug": slug},
    ).result_rows[0][0]


def test_spec_version_defaults_to_one(ch):
    _insert(ch, "default-v")
    assert _read_spec_version(ch, "default-v") == 1


def test_spec_version_accepts_explicit_one(ch):
    _insert(ch, "explicit-1", spec_version=1)
    assert _read_spec_version(ch, "explicit-1") == 1


def test_spec_version_rejects_newer_versions(ch):
    # Decline-newer (issue #16): the MV gates spec_version as a ceiling
    # (> MAX_KNOWN, currently 1). An old MV must refuse a spec newer than
    # it knows rather than store-and-mis-serve it. throwIf aborts the
    # INSERT (CH error 395, message prefixed `bentoclick:`).
    for v in (2, 3, 17, 99, 255):
        with pytest.raises(Exception) as exc:
            _insert(ch, f"future-{v}", spec_version=v)
        msg = str(exc.value)
        assert "395" in msg or "bentoclick: spec_version" in msg, (
            f"v={v} should be declined by the MV; got: {msg}"
        )


def test_spec_version_zero_rejected(ch):
    # 0 is below the supported range [1..MAX_KNOWN] — rejected.
    with pytest.raises(Exception) as exc:
        _insert(ch, "zero-v", spec_version=0)
    msg = str(exc.value)
    assert "395" in msg or "bentoclick: spec_version" in msg, msg


def test_spec_version_overflow_coerces_then_rejected(ch):
    # CH coerces an out-of-range integer to UInt8 at the _raw column
    # (256 -> 0); the MV then declines 0. The net effect is rejection,
    # not a silently-stored garbage version. MCP write-tool callers
    # should still validate spec_version in [1, MAX_KNOWN] up front.
    with pytest.raises(Exception):
        _insert(ch, "overflow", spec_version=256)  # -> 0 -> declined


def test_spec_version_negative_coerces_then_rejected(ch):
    # -1 coerces to 255 at the UInt8 column, which is > MAX_KNOWN -> declined.
    with pytest.raises(Exception):
        _insert(ch, "negative", spec_version=-1)  # -> 255 -> declined


def test_spec_version_column_is_uint8(ch):
    # The column type is the storage-layer guarantee; the MV ceiling is
    # the contract guarantee.
    row = ch.query(
        f"SELECT type FROM system.columns "
        f"WHERE database = %(db)s AND table = 'dashboards' AND name = 'spec_version'",
        parameters={"db": ch.db_name},
    ).result_rows[0]
    assert row[0] == "UInt8"
