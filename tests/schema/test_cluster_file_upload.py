"""Pin scripts/lib.sh's cluster-file-upload pattern.

The `ch_file_upload` helper in scripts/lib.sh (shared by install.sh and
update.sh) deploys SPA assets (spa.js, dash.js, …) to every CH replica by:

  1. CREATE TABLE _asset_<safe_name> (content String)
       ENGINE = File('RawBLOB', '/var/lib/clickhouse/user_files/<path>')
       ON CLUSTER '<cluster>';
  2. INSERT INTO FUNCTION clusterAllReplicas('<cluster>', '<db>', '_asset_<safe_name>')
       SETTINGS engine_file_truncate_on_insert = 1
       SELECT base64Decode('<base64-bytes>');

`clusterAllReplicas` fans the INSERT to every replica of the cluster;
each replica's File-engine table writes the bytes to its OWN local
disk under user_files. The HTTP static handler then serves the file
from local disk per-replica, behind a non-sticky LB.

These tests pin both halves of the pattern. On the single-node test
cluster (`test_cluster` in clickhouse-config.xml), `clusterAllReplicas`
degenerates to one node — but the SQL parses, the table is created,
the bytes round-trip through file(). Multi-replica distribution is
verified in production at deploy time.
"""

from __future__ import annotations

import base64

import pytest


# Use the per-test database from conftest's `ch` fixture, but write
# assets through the test_cluster macro so the cluster-fanout SQL is
# exercised exactly as install.sh writes it.
CLUSTER = "test_cluster"


def _safe_table_name(path: str) -> str:
    """Mirror install.sh's ch_file_upload safe-name derivation."""
    out = []
    for c in path:
        if c.isalnum() or c == "_":
            out.append(c)
        else:
            out.append("_")
    return "_asset_" + "".join(out)


def test_clusterallreplicas_file_round_trips(ch):
    """The full install.sh upload flow: CREATE File-engine table ON
    CLUSTER, INSERT via clusterAllReplicas, read back via file()."""
    rel_path = "test_clusterfile_a.bin"  # under user_files/
    abs_path = f"/var/lib/clickhouse/user_files/{rel_path}"
    table = _safe_table_name(rel_path)
    payload = b"the quick brown fox jumps over the lazy dog"
    b64 = base64.b64encode(payload).decode()

    ch.command(
        f"CREATE TABLE IF NOT EXISTS {ch.db_name}.{table} "
        f"ON CLUSTER '{CLUSTER}' (content String) "
        f"ENGINE = File('RawBLOB', '{abs_path}')"
    )
    ch.command(
        f"INSERT INTO FUNCTION clusterAllReplicas('{CLUSTER}', "
        f"'{ch.db_name}', '{table}') "
        f"SETTINGS engine_file_truncate_on_insert = 1 "
        f"SELECT base64Decode('{b64}')"
    )
    rows = ch.query(
        f"SELECT content FROM file('{rel_path}', 'RawBLOB', 'content String')"
    ).result_rows
    assert len(rows) == 1
    got = rows[0][0]
    if isinstance(got, str):
        got = got.encode()
    assert got == payload, f"file content drift: expected {payload!r}, got {got!r}"


def test_clusterallreplicas_truncate_on_insert_overwrites(ch):
    """install.sh re-uploads on every run; SETTINGS
    engine_file_truncate_on_insert=1 must atomically replace the
    file rather than append."""
    rel_path = "test_clusterfile_b.bin"
    abs_path = f"/var/lib/clickhouse/user_files/{rel_path}"
    table = _safe_table_name(rel_path)
    ch.command(
        f"CREATE TABLE IF NOT EXISTS {ch.db_name}.{table} "
        f"ON CLUSTER '{CLUSTER}' (content String) "
        f"ENGINE = File('RawBLOB', '{abs_path}')"
    )

    def upload(payload: bytes) -> None:
        b64 = base64.b64encode(payload).decode()
        ch.command(
            f"INSERT INTO FUNCTION clusterAllReplicas('{CLUSTER}', "
            f"'{ch.db_name}', '{table}') "
            f"SETTINGS engine_file_truncate_on_insert = 1 "
            f"SELECT base64Decode('{b64}')"
        )

    upload(b"first version")
    upload(b"second version")
    upload(b"third version")
    rows = ch.query(
        f"SELECT content FROM file('{rel_path}', 'RawBLOB', 'content String')"
    ).result_rows
    got = rows[0][0]
    if isinstance(got, str):
        got = got.encode()
    assert got == b"third version", (
        f"truncate-on-insert should leave only the last payload; got {got!r}"
    )


def test_safe_table_name_matches_install_sh():
    """The safe-name derivation in install.sh's ch_file_upload must
    produce a CH-valid identifier. The path 'dash/spa.js' maps to
    '_asset_dash_spa_js', which is what install.sh emits via the
    `tr -c 'A-Za-z0-9_' '_'` pipeline."""
    assert _safe_table_name("dash/spa.js") == "_asset_dash_spa_js"
    assert _safe_table_name("dash/dash-theme.css") == "_asset_dash_dash_theme_css"
    assert _safe_table_name("config.json") == "_asset_config_json"
    # Bracket / weird chars get neutralised but produce a valid
    # identifier (no leading digit, no dot).
    assert _safe_table_name("a.b/c-d.e") == "_asset_a_b_c_d_e"


def test_file_engine_path_must_be_under_user_files(ch):
    """Defensive: CH's File engine refuses to write outside
    user_files_path by default. install.sh always passes paths under
    /var/lib/clickhouse/user_files/, but if a future caller accidentally
    passes /etc/passwd it should fail at CREATE TABLE time, not at
    INSERT. This pins the security boundary."""
    bad_path = "/etc/passwd"
    with pytest.raises(Exception) as exc:
        ch.command(
            f"CREATE TABLE {ch.db_name}._asset_bad ON CLUSTER '{CLUSTER}' "
            f"(content String) ENGINE = File('RawBLOB', '{bad_path}')"
        )
    msg = str(exc.value)
    # CH refuses the path. Error text varies across versions but
    # mentions either "user_files" or a path-access verdict.
    assert (
        "user_files" in msg.lower()
        or "permission" in msg.lower()
        or "path" in msg.lower()
        or "outside" in msg.lower()
        or "not allowed" in msg.lower()
    ), f"expected a path-rejection error, got: {msg}"
