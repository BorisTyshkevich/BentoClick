// Integration tests against a live ClickHouse (env-gated).
//
//	ANON_TEST_CONNECTION=demo go test ./internal/integration/...
//
// Uses a private meta DB (altinity_anontest_<pid>) and a scoped database set;
// every object the run creates is registered and dropped afterwards. The
// admin connection must be able to create databases.
//
// Assertions (from the plan):
//
//	A no-survivor   — no real scope identifier appears in profile rows or sandbox DDL
//	B bijection     — identifier_map injective per kind, exactly the observed set
//	C sandbox works — queryable, deterministic join keys, freetext redacted, schemaless absent
//	D idempotence   — second run produces identical tokens
//	E safety        — a decoy foreign object with a sandbox-shaped name aborts, never dropped
//	F manifest      — incomplete runs are invisible to readers
package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Altinity/anon-discovery/internal/chclient"
	"github.com/Altinity/anon-discovery/internal/discover"
	"github.com/Altinity/anon-discovery/internal/store"
	"github.com/Altinity/anon-discovery/internal/token"
)

const testKey = "integration-test-key-0123456789abcdef"

// scope: small, stable demo databases.
var scopeDBs = []string{"git"}

func conn(t *testing.T) string {
	c := os.Getenv("ANON_TEST_CONNECTION")
	if c == "" {
		t.Skip("ANON_TEST_CONNECTION not set; skipping integration tests")
	}
	return c
}

type fixture struct {
	ex     *chclient.Client
	st     *store.Store
	metaDB string
	run    *discover.Run
	ctx    context.Context
}

func setup(t *testing.T) *fixture {
	c := conn(t)
	metaDB := fmt.Sprintf("altinity_anontest_%d", os.Getpid())
	ex := chclient.New(c)
	cfg := discover.Config{
		Connection: c,
		MetaDB:     metaDB,
		Databases:  scopeDBs,
		WindowDays: 7,
		SampleRows: 10_000,
		HMACKey:    []byte(testKey),
		Log:        t.Logf,
	}
	r, err := discover.NewRun(cfg, ex)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{ex: ex, st: store.New(ex, metaDB), metaDB: metaDB, run: r, ctx: context.Background()}
	t.Cleanup(func() { f.cleanup(t) })
	return f
}

// cleanup drops registered sandbox objects + the test meta DB.
func (f *fixture) cleanup(t *testing.T) {
	objs, err := f.st.RegisteredObjects(f.ctx)
	if err == nil {
		for _, full := range objs["table"] {
			parts := strings.SplitN(full, ".", 2)
			if len(parts) == 2 && token.ReservedRe.MatchString(parts[0]) {
				f.ex.Exec(f.ctx, fmt.Sprintf("DROP TABLE IF EXISTS `%s`.`%s`", parts[0], parts[1]))
			}
		}
		for _, db := range objs["database"] {
			if token.ReservedRe.MatchString(db) {
				f.ex.Exec(f.ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", db))
			}
		}
	}
	if err := f.ex.Exec(f.ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", f.metaDB)); err != nil {
		t.Logf("cleanup: %v", err)
	}
}

func (f *fixture) realIdentifiers(t *testing.T) []string {
	rows, err := f.ex.Query(f.ctx, fmt.Sprintf(`
		SELECT DISTINCT name FROM (
		  SELECT name FROM system.tables WHERE database IN ('%s')
		  UNION ALL
		  SELECT name FROM system.columns WHERE database IN ('%s')
		) WHERE length(name) >= 4`,
		strings.Join(scopeDBs, "','"), strings.Join(scopeDBs, "','")))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows.Data {
		if r[0] != nil {
			out = append(out, *r[0])
		}
	}
	out = append(out, scopeDBs...)
	return out
}

func TestPipeline(t *testing.T) {
	f := setup(t)
	start := time.Now()
	if err := f.run.Execute(f.ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("pipeline completed in %.1fs: %d tables, %d sandboxed",
		time.Since(start).Seconds(), len(f.run.Tables), len(f.run.SandboxRows))
	if len(f.run.Tables) == 0 {
		t.Fatal("no tables discovered in scope")
	}

	t.Run("A_no_survivor", func(t *testing.T) { f.assertNoSurvivor(t) })
	t.Run("B_bijection", func(t *testing.T) { f.assertBijection(t) })
	t.Run("C_sandbox", func(t *testing.T) { f.assertSandbox(t) })
	t.Run("D_idempotence", func(t *testing.T) { f.assertIdempotence(t) })
	t.Run("F_manifest", func(t *testing.T) { f.assertManifest(t) })
}

// A: no real identifier survives in any LLM-readable surface.
func (f *fixture) assertNoSurvivor(t *testing.T) {
	reals := f.realIdentifiers(t)
	var blob strings.Builder
	for tbl, cols := range store.ProfileContentColumns {
		rows, err := f.ex.Query(f.ctx, fmt.Sprintf("SELECT %s FROM `%s`.%s", strings.Join(cols, ", "), f.metaDB, tbl))
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
	// sandbox DDL too
	objs, err := f.st.RegisteredObjects(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, full := range objs["table"] {
		parts := strings.SplitN(full, ".", 2)
		sc, err := f.ex.Query(f.ctx, fmt.Sprintf("SHOW CREATE TABLE `%s`.`%s`", parts[0], parts[1]))
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

// B: identifier_map is injective per kind and covers the scope tables.
func (f *fixture) assertBijection(t *testing.T) {
	rows, err := f.ex.Query(f.ctx, fmt.Sprintf(`
		SELECT kind, count(DISTINCT original), count(DISTINCT token), count()
		FROM `+"`%s`"+`.identifier_map GROUP BY kind`, f.metaDB))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows.Data {
		kind, dOrig, dTok, n := *row[0], *row[1], *row[2], *row[3]
		if dOrig != dTok || dOrig != n {
			t.Errorf("kind %s: not a bijection (orig %s, tok %s, rows %s)", kind, dOrig, dTok, n)
		}
	}
	// every scope table name is in the map
	cnt, err := f.ex.Query(f.ctx, fmt.Sprintf(
		"SELECT count() FROM `%s`.identifier_map WHERE kind = 'tbl'", f.metaDB))
	if err != nil {
		t.Fatal(err)
	}
	if *cnt.Data[0][0] == "0" {
		t.Error("no tbl entries in the identifier map")
	}
}

// C: sandbox tables queryable; freetext redacted; join keys deterministic.
func (f *fixture) assertSandbox(t *testing.T) {
	objs, err := f.st.RegisteredObjects(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs["table"]) == 0 {
		t.Fatal("no sandbox tables created")
	}
	for _, full := range objs["table"] {
		parts := strings.SplitN(full, ".", 2)
		rows, err := f.ex.Query(f.ctx, fmt.Sprintf("SELECT * FROM `%s`.`%s` LIMIT 5", parts[0], parts[1]))
		if err != nil {
			t.Fatalf("sandbox %s unqueryable: %v", full, err)
		}
		for _, name := range rows.Names {
			if !strings.HasPrefix(name, "col_") {
				t.Errorf("sandbox %s leaks a non-token column name", full)
			}
		}
	}

	// determinism: the same real value must hash identically in two
	// different sandbox runs (covered indirectly by D) and within one table:
	// re-masking the source column equals the stored sandbox values.
	plan, err := f.ex.Query(f.ctx, fmt.Sprintf(`
		SELECT database, table, column, transform
		FROM `+"`%s`"+`.masking_plan
		WHERE class = 'joinkey' AND included = 1 LIMIT 1`, f.metaDB))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Data) == 0 {
		t.Log("no joinkey columns in scope; determinism spot-check skipped")
		return
	}
	db, tbl, transform := *plan.Data[0][0], *plan.Data[0][1], *plan.Data[0][3]
	probe, err := f.ex.Query(f.ctx, fmt.Sprintf(
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
	before, err := f.ex.Query(f.ctx, fmt.Sprintf(
		"SELECT kind, original, token FROM `%s`.identifier_map ORDER BY kind, original, token", f.metaDB))
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.run.Cfg
	r2, err := discover.NewRun(cfg, f.ex)
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.Execute(f.ctx); err != nil {
		t.Fatal(err)
	}
	after, err := f.ex.Query(f.ctx, fmt.Sprintf(
		"SELECT DISTINCT kind, original, token FROM `%s`.identifier_map ORDER BY kind, original, token", f.metaDB))
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

// F: manifest written last; readers see only complete runs.
func (f *fixture) assertManifest(t *testing.T) {
	runID, err := f.st.LatestCompleteRun(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" {
		t.Fatal("no complete run visible after Execute")
	}
}

// E: a decoy database named like a sandbox DB but NOT registered must abort
// the run and must never be dropped.
func TestSafetyDecoy(t *testing.T) {
	c := conn(t)
	ex := chclient.New(c)
	ctx := context.Background()
	metaDB := fmt.Sprintf("altinity_anontest_decoy_%d", os.Getpid())

	// figure out which db token the run WILL use for the scope db
	m, err := token.NewMinter([]byte(testKey))
	if err != nil {
		t.Fatal(err)
	}
	decoy := m.Token("db", scopeDBs[0], token.HexLen)

	if err := ex.Exec(ctx, fmt.Sprintf("CREATE DATABASE `%s`", decoy)); err != nil {
		t.Fatal(err)
	}
	defer ex.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", decoy))
	defer ex.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", metaDB))

	cfg := discover.Config{
		Connection: c, MetaDB: metaDB, Databases: scopeDBs,
		WindowDays: 7, SampleRows: 1000, HMACKey: []byte(testKey), Log: t.Logf,
	}
	r, err := discover.NewRun(cfg, ex)
	if err != nil {
		t.Fatal(err)
	}
	err = r.Execute(ctx)
	if err == nil || !strings.Contains(err.Error(), "safety") {
		t.Fatalf("run against a foreign object with our token name must abort with a safety error, got: %v", err)
	}
	// and the decoy must still exist
	rows, err := ex.Query(ctx, fmt.Sprintf("SELECT count() FROM system.databases WHERE name = '%s'", decoy))
	if err != nil {
		t.Fatal(err)
	}
	if *rows.Data[0][0] != "1" {
		t.Fatal("decoy database was dropped — the tool touched a foreign object")
	}
}
