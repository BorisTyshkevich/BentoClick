"""Schema shape tests for ${DB}.dashboards and ${DB}.dashboards_raw."""

from __future__ import annotations


EXPECTED_DASHBOARDS_COLUMNS = {
    # name -> (type, default_kind)
    "slug":         ("String",        ""),
    "title":        ("String",        ""),
    "subtitle":     ("String",        "DEFAULT"),
    "concurrent":   ("Bool",          "DEFAULT"),
    "spec_version": ("UInt8",         "DEFAULT"),
    "params":       ("Array(JSON)",   "DEFAULT"),
    "panels":       ("Array(JSON)",   "DEFAULT"),
    "meta":         ("JSON",          "DEFAULT"),
    "tags":         ("Array(String)", "DEFAULT"),
    "owner":        ("String",        "DEFAULT"),
    "updated_at":   ("DateTime",      "DEFAULT"),
}

# dashboards_raw stores the JSON payload columns as plain String (the
# agent passes JSON-encoded text). The MV parses these on the way to
# `dashboards`. Owner and updated_at are MV-supplied — not on raw.
EXPECTED_RAW_COLUMNS = {
    "slug":         ("String", ""),
    "title":        ("String", ""),
    "subtitle":     ("String", "DEFAULT"),
    "concurrent":   ("Bool",   "DEFAULT"),
    "spec_version": ("UInt8",  "DEFAULT"),
    "params":       ("String", "DEFAULT"),
    "panels":       ("String", "DEFAULT"),
    "meta":         ("String", "DEFAULT"),
    "tags":         ("String", "DEFAULT"),
}


def _columns(ch, table: str) -> dict[str, tuple[str, str]]:
    rows = ch.query(
        "SELECT name, type, default_kind FROM system.columns "
        "WHERE database = %(db)s AND table = %(t)s",
        parameters={"db": ch.db_name, "t": table},
    ).result_rows
    return {name: (type_, dk) for name, type_, dk in rows}


def test_dashboards_columns_match_spec(ch):
    got = _columns(ch, "dashboards")
    assert got == EXPECTED_DASHBOARDS_COLUMNS, (
        f"dashboards columns drifted:\n  expected: {EXPECTED_DASHBOARDS_COLUMNS}\n  got:      {got}"
    )


def test_dashboards_raw_columns_match_spec(ch):
    got = _columns(ch, "dashboards_raw")
    assert got == EXPECTED_RAW_COLUMNS, (
        f"dashboards_raw columns drifted:\n  expected: {EXPECTED_RAW_COLUMNS}\n  got:      {got}"
    )


def test_dashboards_raw_has_no_owner_or_updated_at(ch):
    got = _columns(ch, "dashboards_raw")
    forbidden = set(got.keys()) & {"owner", "updated_at"}
    assert not forbidden, (
        f"dashboards_raw must not expose owner/updated_at; found: {forbidden}"
    )


def test_dashboards_engine_is_replicated_replacing_merge_tree(ch):
    row = ch.query(
        "SELECT engine, engine_full FROM system.tables "
        "WHERE database = %(db)s AND name = 'dashboards'",
        parameters={"db": ch.db_name},
    ).result_rows[0]
    engine, engine_full = row
    assert engine == "ReplicatedReplacingMergeTree", f"engine: {engine}"
    assert "updated_at" in engine_full, (
        f"engine_full must reference updated_at as the version column: {engine_full}"
    )


def test_dashboards_raw_engine_is_null(ch):
    row = ch.query(
        "SELECT engine FROM system.tables "
        "WHERE database = %(db)s AND name = 'dashboards_raw'",
        parameters={"db": ch.db_name},
    ).result_rows[0]
    assert row[0] == "Null", f"engine: {row[0]}"


def test_dashboards_sorting_key_is_owner_slug(ch):
    row = ch.query(
        "SELECT sorting_key FROM system.tables "
        "WHERE database = %(db)s AND name = 'dashboards'",
        parameters={"db": ch.db_name},
    ).result_rows[0]
    sorting_key = row[0].replace(" ", "")
    assert sorting_key == "owner,slug", f"sorting_key: {row[0]}"


def test_dashboards_mv_wires_raw_to_dashboards(ch):
    row = ch.query(
        "SELECT engine, as_select FROM system.tables "
        "WHERE database = %(db)s AND name = 'dashboards_mv'",
        parameters={"db": ch.db_name},
    ).result_rows[0]
    engine, as_select = row
    assert engine == "MaterializedView", f"engine: {engine}"
    assert "dashboards_raw" in as_select, (
        f"MV must SELECT FROM dashboards_raw, got: {as_select}"
    )

    # TO-target: system.tables stores it on the inner .inner_id_xxx table or
    # via `target_table` in CH 24+. Verify by checking that an insert into
    # dashboards_raw ends up in dashboards (separate test below).


def test_dashboards_mv_uses_security_definer(ch):
    """The MV must run as ${DB}_definer with SQL SECURITY DEFINER so
    it can INSERT into `dashboards` even when no other role has that
    grant. Pin both the clause and the definer username via the
    `create_table_query` column on system.tables."""
    rows = ch.query(
        "SELECT create_table_query FROM system.tables "
        "WHERE database = %(db)s AND name = 'dashboards_mv'",
        parameters={"db": ch.db_name},
    ).result_rows
    assert rows, "dashboards_mv should exist"
    ddl = rows[0][0]
    assert "SQL SECURITY DEFINER" in ddl, (
        f"MV missing SQL SECURITY DEFINER clause:\n{ddl}"
    )
    assert f"{ch.db_name}_definer" in ddl, (
        f"MV missing DEFINER = {ch.db_name}_definer:\n{ddl}"
    )
    assert "currentUser()" in ddl, (
        f"MV must record owner via currentUser() — empirically returns "
        f"the session user even under DEFINER, which is what we want:\n{ddl}"
    )


def test_insert_into_raw_propagates_to_dashboards(ch):
    # Sanity: insert into dashboards_raw, see a row appear in dashboards
    # with owner = currentUser() (the session user; the SQL SECURITY
    # DEFINER MV's currentUser() returns the session user in CH, not
    # the definer — verified empirically against CH 26.3). The agent
    # passes JSON-encoded text strings; the MV parses on the way in.
    #
    # NOTE: uses INSERT ... SELECT form intentionally. ClickHouse 26.x
    # has a bug where currentUser() inside any expression evaluated
    # after an INSERT ... VALUES (including DEFAULTs and MV SELECT
    # bodies) returns '' instead of the actual user. The MCP reflected
    # write tool emits INSERT statements that flow through the SELECT
    # path so this is the realistic call shape.
    ch.command(
        f"INSERT INTO {ch.db_name}.dashboards_raw "
        "(slug, title, subtitle, concurrent, spec_version, params, panels, meta, tags) "
        "SELECT 'smoke', 'Smoke', 'hi', false, 1, '[]', '[]', '{}', '[]'"
    )
    rows = ch.query(
        f"SELECT slug, title, owner, spec_version, "
        f"       toUnixTimestamp(updated_at) > 0 AS has_ts "
        f"FROM {ch.db_name}.dashboards FINAL WHERE slug = 'smoke'"
    ).result_rows
    assert len(rows) == 1, f"expected 1 row in dashboards after raw insert, got: {rows}"
    slug, title, owner, spec_version, has_ts = rows[0]
    assert (slug, title) == ("smoke", "Smoke")
    assert owner == "default", (
        f"owner should be currentUser() = default (the connecting test user, "
        f"NOT the SECURITY DEFINER user); got {owner!r}"
    )
    assert spec_version == 1
    assert has_ts == 1


def test_insert_values_form_owner_quirk(ch):
    """Regression check on the CH 26.x quirk: INSERT...VALUES leaves
    owner empty. If this test starts failing because owner = 'default',
    the upstream bug is fixed and the SELECT-form note in
    docs/SANITIZATION.md / the skill can be dropped."""
    ch.command(
        f"INSERT INTO {ch.db_name}.dashboards_raw "
        "(slug, title) VALUES ('quirk', 'Q')"
    )
    rows = ch.query(
        f"SELECT owner FROM {ch.db_name}.dashboards FINAL WHERE slug = 'quirk'"
    ).result_rows
    assert rows, "row should still land via the MV"
    if rows[0][0] != "":
        import pytest
        pytest.xfail(
            "CH currentUser()-in-VALUES quirk appears fixed: "
            f"owner = {rows[0][0]!r}. Drop the SELECT-form requirement."
        )
    assert rows[0][0] == "", (
        "expected the documented CH quirk to leave owner empty on "
        f"INSERT...VALUES; got {rows[0][0]!r}"
    )


def test_dashboards_prefix_view_returns_expected_fields(ch):
    rows = ch.query(
        f"SELECT owner, spa_origin, my_dashboards_prefix "
        f"FROM {ch.db_name}.dashboards_prefix"
    ).result_rows
    assert len(rows) == 1
    owner, spa_origin, prefix = rows[0]
    # Test runs as the `default` user; no `@` to encode.
    assert owner == "default"
    assert spa_origin.startswith("http"), f"spa_origin: {spa_origin}"
    assert prefix == f"{spa_origin}/b/{owner}/"


def test_dashboards_prefix_encodes_at_sign(ch):
    # The view's URL is built via encodeURLComponent(currentUser()); when
    # the user is `default` there's no `@` to encode, so verify the
    # encoding function itself round-trips an email-style identity.
    row = ch.query(
        "SELECT encodeURLComponent('foo@bar.com')"
    ).result_rows[0]
    assert row[0] == "foo%40bar.com"
