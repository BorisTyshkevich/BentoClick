// Integration tests against live ClickHouse (env-gated).
//
// Single-cluster suite (source = dest = one cluster):
//
//	ANON_TEST_CONNECTION=demo go test ./internal/integration/...
//
// or pass the full client command directly (CI uses this — no named-connection
// config needed):
//
//	ANON_TEST_CMD="clickhouse-client --host 127.0.0.1 --port 9000" go test ./internal/integration/...
//
// Cross-cluster suite (source via a wrapper command, dest via a connection):
//
//	ANON_TEST_SOURCE_CMD="cl otel" ANON_TEST_SOURCE_DB=claude_otel \
//	ANON_TEST_DEST_CONNECTION=demo go test -run TestCrossCluster ./internal/integration/...
//
// Tests use a private meta DB (altinity_anontest_<pid>) and a test-named dest
// sandbox DB; every object the run creates is registered and dropped
// afterwards. The connections must be able to create databases.
//
// Assertions:
//
//	A no-survivor   — no real scope identifier appears in dest profile rows or sandbox DDL
//	B bijection     — identifier_map (SOURCE) injective per kind
//	C sandbox works — queryable on dest, token columns, deterministic transforms
//	D idempotence   — second run produces identical tokens
//	E safety        — a decoy foreign object with our dest DB name aborts, never dropped
//	F manifest      — incomplete runs are invisible to readers
//	G trusted split — identifier_map / masking_plan never exist on the dest (cross-cluster)
//	H attrmap       — masked map values are numeric/bool/empty/12-hex only (cross-cluster)
package integration

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Altinity/anon-discovery/internal/chclient"
	"github.com/Altinity/anon-discovery/internal/discover"
	"github.com/Altinity/anon-discovery/internal/store"
)

const testKey = "integration-test-key-0123456789abcdef"

type fixture struct {
	srcEx, dstEx chclient.Executor
	dstStore     *store.Store
	srcStore     *store.Store
	metaDB       string
	cfg          discover.Config
	run          *discover.Run
	ctx          context.Context
	crossCluster bool
	seededSrcDB  string // non-empty when the test created the source DB (drop it on cleanup)
}

// singleClientCmd resolves the clickhouse-client command for the single-cluster
// suite. ANON_TEST_CMD wins (a full client command, e.g.
// "clickhouse-client --host 127.0.0.1 --port 9000") — it sidesteps the
// named-connection config, which is what CI uses. Otherwise ANON_TEST_CONNECTION
// builds "clickhouse-client --connection <name>". Skips if neither is set.
func singleClientCmd(t *testing.T) string {
	if cmd := os.Getenv("ANON_TEST_CMD"); cmd != "" {
		return cmd
	}
	if c := os.Getenv("ANON_TEST_CONNECTION"); c != "" {
		return "clickhouse-client --connection " + c
	}
	t.Skip("ANON_TEST_CMD / ANON_TEST_CONNECTION not set; skipping integration tests")
	return ""
}

// setupSingle: source = dest = the single-cluster client. Self-seeds a small,
// representative source DB so the suite is portable (no pre-existing dataset
// needed) — runs identically locally and in CI.
func setupSingle(t *testing.T) *fixture {
	cmd := singleClientCmd(t)
	srcDB := fmt.Sprintf("anontest_src_%d", os.Getpid())
	seedSource(t, chclient.NewFromString(cmd), srcDB)
	f := setup(t, cmd, cmd, srcDB, fmt.Sprintf("anontest_sb_%d", os.Getpid()), false)
	f.seededSrcDB = srcDB
	return f
}

// seedSource builds a tiny git-commits-shaped dataset exercising the masking
// classes: a join key shared across two tables (author_email), a high-card hash
// (joinkey), free text (redact), names, low-card vocabulary, measures, time.
func seedSource(t *testing.T, ex chclient.Executor, db string) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", db),
		fmt.Sprintf("CREATE DATABASE `%s`", db),
		fmt.Sprintf("CREATE TABLE `%s`.commits ("+
			"commit_hash String, author_name String, author_email String, "+
			"message String, added_lines UInt32, repo_name LowCardinality(String), "+
			"commit_time DateTime) ENGINE = MergeTree ORDER BY commit_time", db),
		fmt.Sprintf("INSERT INTO `%s`.commits SELECT "+
			"lower(hex(murmurHash3_128(toString(number)))), concat('author_', toString(number %% 37)), "+
			"concat('user', toString(number %% 37), '@example.com'), "+
			"concat('commit message ', toString(number)), toUInt32(number %% 900), "+
			"['alpha','beta','gamma'][1 + (number %% 3)], now() - toIntervalSecond(number) "+
			"FROM numbers(3000)", db),
		fmt.Sprintf("CREATE TABLE `%s`.authors ("+
			"author_email String, display_name String, commits_count UInt64) "+
			"ENGINE = MergeTree ORDER BY author_email", db),
		fmt.Sprintf("INSERT INTO `%s`.authors SELECT "+
			"concat('user', toString(number), '@example.com'), "+
			"concat('Author ', toString(number)), toUInt64(number * 7) FROM numbers(37)", db),
		// make system.query_log exist + flush so the mining phase has input
		"SYSTEM FLUSH LOGS",
	}
	for _, s := range stmts {
		if err := ex.Exec(ctx, s); err != nil {
			t.Fatalf("seed source DB %q: %v\n  stmt: %s", db, err, s)
		}
	}
}

// seedRegistry pre-creates the LLM-facing registry *_data tables (normally
// provisioned by anon/integrations/bentoclick/sql/08-schema-guide-registry.sql)
// in the per-test registry DB, so the pipeline's registry write lands like a
// real deploy. DDL mirrors 08-…sql (minus the views/comments/grants).
func seedRegistry(t *testing.T, ex chclient.Executor, db string) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", db),
		fmt.Sprintf("CREATE TABLE IF NOT EXISTS `%s`.schema_guide_data ("+
			"run_id String, anon_database String, "+
			"model Enum8('tokenizing'=1,'schema-preserving'=2), "+
			"naming Enum8('tokens'=1,'real'=2), "+
			"table_name String, table_role String DEFAULT '', "+
			"total_rows UInt64 DEFAULT 0, sandbox_rows UInt64 DEFAULT 0, position UInt32 DEFAULT 0, "+
			"column_name String, type String DEFAULT '', "+
			"class Enum8('real'=1,'identifier'=2,'redacted'=3,'attrmap'=4), "+
			"usage String DEFAULT '', updated_at DateTime DEFAULT now()) "+
			"ENGINE = ReplacingMergeTree(updated_at) ORDER BY (anon_database, table_name, column_name)", db),
		fmt.Sprintf("CREATE TABLE IF NOT EXISTS `%s`.attr_guide_data ("+
			"run_id String, anon_database String, table_name String, column_name String, attr_key String, "+
			"role Enum8('vocabulary'=1,'measure'=2,'identity'=3,'sensitive'=4), "+
			"usage String DEFAULT '', updated_at DateTime DEFAULT now()) "+
			"ENGINE = ReplacingMergeTree(updated_at) ORDER BY (anon_database, table_name, column_name, attr_key)", db),
	}
	for _, s := range stmts {
		if err := ex.Exec(ctx, s); err != nil {
			t.Fatalf("seed registry DB %q: %v\n  stmt: %s", db, err, s)
		}
	}
}

// setupCross: source = ANON_TEST_SOURCE_CMD (db ANON_TEST_SOURCE_DB, default
// claude_otel), dest = ANON_TEST_DEST_CONNECTION.
func setupCross(t *testing.T) *fixture {
	src := os.Getenv("ANON_TEST_SOURCE_CMD")
	dst := os.Getenv("ANON_TEST_DEST_CONNECTION")
	if src == "" || dst == "" {
		t.Skip("ANON_TEST_SOURCE_CMD / ANON_TEST_DEST_CONNECTION not set; skipping cross-cluster tests")
	}
	srcDB := os.Getenv("ANON_TEST_SOURCE_DB")
	if srcDB == "" {
		srcDB = "claude_otel"
	}
	return setup(t, src, "clickhouse-client --connection "+dst, srcDB,
		fmt.Sprintf("anontest_xc_%d", os.Getpid()), true)
}

func setup(t *testing.T, srcCmd, dstCmd, srcDB, destDB string, cross bool) *fixture {
	metaDB := fmt.Sprintf("altinity_anontest_%d", os.Getpid())
	cfg := discover.Config{
		Source:     srcCmd,
		Dest:       dstCmd,
		SourceDB:   srcDB,
		DestDB:     destDB, // differs from srcDB -> DB name stays tokenized
		MetaDB:     metaDB,
		SecretDB:   metaDB, // co-locate the secret in the per-test meta DB (dropped on cleanup)
		RegistryDB: metaDB, // ditto for the LLM-facing registry (normally bentoclick)
		WindowDays: 7,
		SampleRows: 10_000,
		HMACKey:    []byte(testKey),
		Log:        t.Logf,
	}
	srcEx := chclient.NewFromString(srcCmd)
	dstEx := chclient.NewFromString(dstCmd)
	r, err := discover.NewRun(cfg, srcEx, dstEx)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		srcEx: srcEx, dstEx: dstEx,
		srcStore: store.New(srcEx, metaDB), dstStore: store.New(dstEx, metaDB),
		metaDB: metaDB, cfg: cfg, run: r, ctx: context.Background(), crossCluster: cross,
	}
	t.Cleanup(func() { f.cleanup(t) })
	// the registry *_data tables are deploy-provisioned (08-…sql); create them
	// in the per-test registry DB (dest cluster) so the pipeline's write lands.
	seedRegistry(t, dstEx, cfg.RegistryDB)
	return f
}

// cleanup drops registered dest objects + the test meta DB on both sides.
func (f *fixture) cleanup(t *testing.T) {
	objs, err := f.dstStore.RegisteredObjects(f.ctx)
	if err == nil {
		for _, full := range objs["table"] {
			if db, tbl, ok := strings.Cut(full, "."); ok {
				f.dstEx.Exec(f.ctx, fmt.Sprintf("DROP TABLE IF EXISTS `%s`.`%s`", db, tbl))
			}
		}
		for _, db := range objs["database"] {
			f.dstEx.Exec(f.ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", db))
		}
	}
	if err := f.dstEx.Exec(f.ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", f.metaDB)); err != nil {
		t.Logf("cleanup dest meta: %v", err)
	}
	if err := f.srcEx.Exec(f.ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", f.metaDB)); err != nil {
		t.Logf("cleanup source meta: %v", err)
	}
	if f.seededSrcDB != "" {
		if err := f.srcEx.Exec(f.ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", f.seededSrcDB)); err != nil {
			t.Logf("cleanup seeded source: %v", err)
		}
	}
}

// realIdentifiers: table + column names of the source DB (len >= 4).
func (f *fixture) realIdentifiers(t *testing.T) []string {
	db := strings.ReplaceAll(f.cfg.SourceDB, "'", "\\'")
	rows, err := f.srcEx.Query(f.ctx, fmt.Sprintf(`
		SELECT DISTINCT name FROM (
		  SELECT name FROM system.tables WHERE database = '%s'
		  UNION ALL
		  SELECT name FROM system.columns WHERE database = '%s'
		) WHERE length(name) >= 4`, db, db))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows.Data {
		if r[0] != nil {
			out = append(out, *r[0])
		}
	}
	return append(out, f.cfg.SourceDB)
}

func TestPipeline(t *testing.T) {
	f := setupSingle(t)
	f.execute(t)
	f.runAssertions(t)
}

func TestCrossCluster(t *testing.T) {
	f := setupCross(t)
	f.execute(t)
	f.runAssertions(t)
	t.Run("G_trusted_split", func(t *testing.T) { f.assertTrustedSplit(t) })
	t.Run("H_attrmap", func(t *testing.T) { f.assertAttrMap(t) })
}

func (f *fixture) execute(t *testing.T) {
	start := time.Now()
	if err := f.run.Execute(f.ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("pipeline completed in %.1fs: %d tables, %d sandboxed",
		time.Since(start).Seconds(), len(f.run.Tables), len(f.run.SandboxRows))
	if len(f.run.Tables) == 0 {
		t.Fatal("no tables discovered in scope")
	}
}

func (f *fixture) runAssertions(t *testing.T) {
	t.Run("A_no_survivor", func(t *testing.T) { f.assertNoSurvivor(t) })
	t.Run("B_bijection", func(t *testing.T) { f.assertBijection(t) })
	t.Run("C_sandbox", func(t *testing.T) { f.assertSandbox(t) })
	t.Run("D_idempotence", func(t *testing.T) { f.assertIdempotence(t) })
	t.Run("F_manifest", func(t *testing.T) { f.assertManifest(t) })
}

// A: no real identifier survives in any LLM-readable surface on the DEST.
func (f *fixture) assertNoSurvivor(t *testing.T) {
	reals := f.realIdentifiers(t)
	var blob strings.Builder
	for tbl, cols := range store.ProfileContentColumns {
		rows, err := f.dstEx.Query(f.ctx, fmt.Sprintf(
			"SELECT %s FROM `%s`.%s", strings.Join(cols, ", "), f.metaDB, tbl))
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows.Data {
			for _, c := range row {
				if c != nil {
					blob.WriteString(*c)
					blob.WriteByte('\n')
				}
			}
		}
	}
	objs, err := f.dstStore.RegisteredObjects(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, full := range objs["table"] {
		db, tbl, _ := strings.Cut(full, ".")
		sc, err := f.dstEx.Query(f.ctx, fmt.Sprintf("SHOW CREATE TABLE `%s`.`%s`", db, tbl))
		if err != nil {
			t.Fatal(err)
		}
		if len(sc.Data) > 0 && sc.Data[0][0] != nil {
			blob.WriteString(*sc.Data[0][0])
		}
	}
	text := blob.String()
	survivors := 0
	for _, real := range reals {
		if discover.WordPresent(text, real) {
			survivors++
			t.Errorf("real identifier survived (len %d): %s...", len(real), real[:3])
		}
	}
	t.Logf("checked %d identifiers, %d survivors", len(reals), survivors)
}

// B: identifier_map on the SOURCE is injective per kind.
func (f *fixture) assertBijection(t *testing.T) {
	rows, err := f.srcEx.Query(f.ctx, fmt.Sprintf(`
		SELECT kind, count(DISTINCT original), count(DISTINCT token), count()
		FROM `+"`%s`"+`.identifier_map GROUP BY kind`, f.metaDB))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Data) == 0 {
		t.Fatal("identifier_map empty on source")
	}
	for _, row := range rows.Data {
		kind, dOrig, dTok, n := *row[0], *row[1], *row[2], *row[3]
		if dOrig != dTok || dOrig != n {
			t.Errorf("kind %s: not a bijection (orig %s, tok %s, rows %s)", kind, dOrig, dTok, n)
		}
	}
}

// C: sandbox tables on dest queryable with token columns; transforms deterministic.
func (f *fixture) assertSandbox(t *testing.T) {
	objs, err := f.dstStore.RegisteredObjects(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs["table"]) == 0 {
		t.Fatal("no sandbox tables created")
	}
	for _, full := range objs["table"] {
		db, tbl, _ := strings.Cut(full, ".")
		rows, err := f.dstEx.Query(f.ctx, fmt.Sprintf("SELECT * FROM `%s`.`%s` LIMIT 5", db, tbl))
		if err != nil {
			t.Fatalf("sandbox %s unqueryable: %v", full, err)
		}
		for _, name := range rows.Names {
			if !strings.HasPrefix(name, "col_") {
				t.Errorf("sandbox %s leaks a non-token column name", full)
			}
		}
	}
	// determinism: re-evaluating a joinkey transform on the SOURCE equals itself
	plan, err := f.srcEx.Query(f.ctx, fmt.Sprintf(`
		SELECT database, table, transform
		FROM `+"`%s`"+`.masking_plan
		WHERE class = 'joinkey' AND included = 1 LIMIT 1`, f.metaDB))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Data) == 0 {
		t.Log("no joinkey columns in scope; determinism spot-check skipped")
		return
	}
	db, tbl, transform := *plan.Data[0][0], *plan.Data[0][1], *plan.Data[0][2]
	probe, err := f.srcEx.Query(f.ctx, fmt.Sprintf(
		"SELECT %s AS a, %s AS b FROM `%s`.`%s` LIMIT 1", transform, transform, db, tbl))
	if err != nil {
		t.Fatalf("transform probe: %v", err)
	}
	if len(probe.Data) > 0 {
		a, b := probe.Data[0][0], probe.Data[0][1]
		if (a == nil) != (b == nil) || (a != nil && *a != *b) {
			t.Error("masking transform is not deterministic")
		}
	}
}

// D: a second run mints identical tokens (HMAC determinism end-to-end).
func (f *fixture) assertIdempotence(t *testing.T) {
	mapSQL := fmt.Sprintf(
		"SELECT DISTINCT kind, original, token FROM `%s`.identifier_map ORDER BY kind, original, token", f.metaDB)
	before, err := f.srcEx.Query(f.ctx, mapSQL)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := discover.NewRun(f.cfg, f.srcEx, f.dstEx)
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.Execute(f.ctx); err != nil {
		t.Fatal(err)
	}
	after, err := f.srcEx.Query(f.ctx, mapSQL)
	if err != nil {
		t.Fatal(err)
	}
	// distinct triples across both runs must equal the first run's set:
	// same (kind, original) -> same token.
	if len(before.Data) != len(after.Data) {
		t.Fatalf("token sets differ between runs: %d vs %d distinct triples", len(before.Data), len(after.Data))
	}
	for i := range before.Data {
		for j := range before.Data[i] {
			if *before.Data[i][j] != *after.Data[i][j] {
				t.Fatalf("token drift at row %d", i)
			}
		}
	}
}

// F: manifest (on DEST) written last; readers see only complete runs.
func (f *fixture) assertManifest(t *testing.T) {
	runID, err := f.dstStore.LatestCompleteRun(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" {
		t.Fatal("no complete run visible after Execute")
	}
}

// G: the trusted tables never exist on the dest cluster's meta DB.
func (f *fixture) assertTrustedSplit(t *testing.T) {
	for _, tbl := range store.TrustedTables {
		rows, err := f.dstEx.Query(f.ctx, fmt.Sprintf(
			"SELECT count() FROM system.tables WHERE database = '%s' AND name = '%s'", f.metaDB, tbl))
		if err != nil {
			t.Fatal(err)
		}
		if *rows.Data[0][0] != "0" {
			t.Errorf("trusted table %s exists on the DEST cluster", tbl)
		}
	}
}

// H: masked attrmap values on the dest are only numeric/bool/empty/12-hex.
func (f *fixture) assertAttrMap(t *testing.T) {
	// find an attrmap column in the dest profile (class is stored per column)
	rows, err := f.dstEx.Query(f.ctx, fmt.Sprintf(`
		SELECT db_token, table_token, col_token FROM `+"`%s`"+`.profile_columns
		WHERE class = 'attrmap' AND included = 1 LIMIT 1`, f.metaDB))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Data) == 0 {
		t.Skip("no attrmap columns in scope")
	}
	tblTok, colTok := *rows.Data[0][1], *rows.Data[0][2]
	vals, err := f.dstEx.Query(f.ctx, fmt.Sprintf(
		"SELECT arrayJoin(mapValues(%s)) AS v FROM `%s`.`%s` LIMIT 200", colTok, f.cfg.DestDB, tblTok))
	if err != nil {
		t.Fatalf("attrmap values query: %v", err)
	}
	okVal := regexp.MustCompile(`^(|-?[0-9.]+|true|false|[0-9a-f]{12})$`)
	bad := 0
	for _, row := range vals.Data {
		if row[0] != nil && !okVal.MatchString(*row[0]) {
			bad++
		}
	}
	if bad > 0 {
		t.Errorf("%d attrmap values are neither kept-safe nor 12-hex masked", bad)
	}
	t.Logf("attrmap spot-check: %d values OK", len(vals.Data))
}

// E: a decoy database named like our dest DB but NOT registered must abort
// the run and must never be dropped.
func TestSafetyDecoy(t *testing.T) {
	cmd := singleClientCmd(t)
	ex := chclient.NewFromString(cmd)
	ctx := context.Background()
	metaDB := fmt.Sprintf("altinity_anontest_decoy_%d", os.Getpid())
	decoy := fmt.Sprintf("anontest_decoy_%d", os.Getpid())

	if err := ex.Exec(ctx, fmt.Sprintf("CREATE DATABASE `%s`", decoy)); err != nil {
		t.Fatal(err)
	}
	defer ex.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", decoy))
	defer ex.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", metaDB))

	// a real source so the run gets PAST roster and reaches the dest-DB safety
	// check (the decoy is a foreign object with our dest name → must abort).
	srcDB := fmt.Sprintf("anontest_decoy_src_%d", os.Getpid())
	seedSource(t, ex, srcDB)
	defer ex.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", srcDB))

	cfg := discover.Config{
		Source: cmd, Dest: cmd, SourceDB: srcDB, DestDB: decoy,
		MetaDB: metaDB, SecretDB: metaDB, WindowDays: 7, SampleRows: 1000,
		HMACKey: []byte(testKey), Log: t.Logf,
	}
	r, err := discover.NewRun(cfg, ex, ex)
	if err != nil {
		t.Fatal(err)
	}
	err = r.Execute(ctx)
	if err == nil || !strings.Contains(err.Error(), "safety") {
		t.Fatalf("run against a foreign dest DB must abort with a safety error, got: %v", err)
	}
	rows, err := ex.Query(ctx, fmt.Sprintf("SELECT count() FROM system.databases WHERE name = '%s'", decoy))
	if err != nil {
		t.Fatal(err)
	}
	if *rows.Data[0][0] != "1" {
		t.Fatal("decoy database was dropped — the tool touched a foreign object")
	}
}
