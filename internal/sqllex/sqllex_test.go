package sqllex

import (
	"strings"
	"testing"

	"github.com/Altinity/anon-discovery/internal/idmap"
	"github.com/Altinity/anon-discovery/internal/token"
)

// fixture: a map with known identifiers and a rewriter over it.
func fixture(t *testing.T) (*idmap.IdMap, *Rewriter) {
	m, err := token.NewMinter([]byte("test-key-0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	im := idmap.New(m)
	im.KeepVerbatim("db", "system")
	im.KeepVerbatim("db", "default")
	for kind, vals := range map[string][]string{
		"db":   {"shop"},
		"tbl":  {"orders", "customers"},
		"col":  {"order_id", "customer_email", "created_at"},
		"user": {"alice"},
	} {
		for _, v := range vals {
			im.Observe(kind, v)
		}
	}
	rw := NewRewriter(im, NewKeepRegistry([]string{"toDate", "uniqExact", "max_threads"}))
	// observe the sql wave like the pipeline does
	known := im.Known(Probe)
	for _, sql := range testCorpus {
		for _, w := range rw.IdentifierWords(sql) {
			if !known[w] {
				im.Observe("sql", w)
			}
		}
	}
	if err := im.Build(); err != nil {
		t.Fatal(err)
	}
	return im, rw
}

var testCorpus = []string{
	`SELECT order_id, customer_email FROM shop.orders WHERE created_at > '2024-01-01' -- daily check`,
	`SELECT count() FROM shop.orders o JOIN shop.customers c ON o.order_id = c.order_id`,
	"SELECT `customer_email` FROM orders /* inline comment */ LIMIT 10",
	`SELECT toDate(created_at) AS d, uniqExact(order_id) FROM shop.orders GROUP BY d SETTINGS max_threads = 8`,
	`SELECT secret_column FROM unknown_table`,
}

func mustTok(t *testing.T, im *idmap.IdMap, kind, v string) string {
	tok, err := im.Map(kind, v)
	if err != nil {
		t.Fatalf("map %s %s: %v", kind, v, err)
	}
	return tok
}

func TestRewriteBasics(t *testing.T) {
	im, rw := fixture(t)
	out := rw.Rewrite(testCorpus[0], false)

	for _, leaked := range []string{"shop", "orders", "order_id", "customer_email", "created_at", "2024-01-01", "daily check"} {
		if strings.Contains(out, leaked) {
			t.Errorf("rewrite leaked %q: %s", leaked, out)
		}
	}
	for _, kept := range []string{"SELECT", "FROM", "WHERE"} {
		if !strings.Contains(out, kept) {
			t.Errorf("rewrite lost keyword %q: %s", kept, out)
		}
	}
	if !strings.Contains(out, mustTok(t, im, "tbl", "orders")) {
		t.Errorf("orders must be replaced by its structured token: %s", out)
	}
	if !strings.Contains(out, Redacted) {
		t.Errorf("string literal must be redacted: %s", out)
	}
}

func TestRewriteConsistencyWithStructuredTokens(t *testing.T) {
	im, rw := fixture(t)
	// the same identifier in SQL and in structured columns must agree
	colTok := mustTok(t, im, "col", "order_id")
	out := rw.Rewrite(testCorpus[1], false)
	if !strings.Contains(out, colTok) {
		t.Errorf("order_id in SQL must reuse the structured col token %s: %s", colTok, out)
	}
}

func TestQuotedIdentifiersAlwaysMapped(t *testing.T) {
	im, rw := fixture(t)
	out := rw.Rewrite(testCorpus[2], false)
	if strings.Contains(out, "customer_email") {
		t.Errorf("backticked identifier leaked: %s", out)
	}
	if !strings.Contains(out, mustTok(t, im, "col", "customer_email")) {
		t.Errorf("backticked identifier must map to structured token: %s", out)
	}
	if strings.Contains(out, "inline comment") {
		t.Errorf("block comment leaked: %s", out)
	}
}

func TestVocabularyKept(t *testing.T) {
	_, rw := fixture(t)
	out := rw.Rewrite(testCorpus[3], false)
	for _, kept := range []string{"toDate", "uniqExact", "max_threads", "GROUP BY", "SETTINGS"} {
		if !strings.Contains(out, kept) {
			t.Errorf("cluster vocabulary %q must be kept: %s", kept, out)
		}
	}
	if !strings.Contains(out, "= 8") {
		t.Errorf("numeric literal must survive non-strict rewrite: %s", out)
	}
}

func TestStrictRedactsNumbers(t *testing.T) {
	_, rw := fixture(t)
	out := rw.Rewrite("WHERE account_id = 5551234567", true)
	if strings.Contains(out, "5551234567") {
		t.Errorf("strict mode must redact numbers: %s", out)
	}
}

func TestUnknownIdentifiersGetSQLTokens(t *testing.T) {
	_, rw := fixture(t)
	out := rw.Rewrite(testCorpus[4], false)
	for _, leaked := range []string{"secret_column", "unknown_table"} {
		if strings.Contains(out, leaked) {
			t.Errorf("unknown identifier leaked: %s", out)
		}
	}
	if !strings.Contains(out, "sql_") {
		t.Errorf("unknown identifiers must become sql_ tokens: %s", out)
	}
}

func TestFailClosedOnUnbalancedQuote(t *testing.T) {
	_, rw := fixture(t)
	out := rw.Rewrite(`SELECT 'unterminated FROM shop.orders`, false)
	if out != Redacted {
		t.Errorf("unbalanced quote must redact the whole value, got: %s", out)
	}
}

func TestFailClosedOnUnobservedSQLIdentifier(t *testing.T) {
	// a rewriter whose observe wave never saw the input: must redact, not leak
	_, rw := fixture(t)
	out := rw.Rewrite("SELECT never_observed_xyz FROM also_never_seen", false)
	for _, leaked := range []string{"never_observed_xyz", "also_never_seen"} {
		if strings.Contains(out, leaked) {
			t.Errorf("unobserved identifier must not survive: %s", out)
		}
	}
}

func TestSafeLiteralsKept(t *testing.T) {
	_, rw := fixture(t)
	out := rw.Rewrite("SELECT toStartOfInterval(created_at, INTERVAL 1 'day'), '123', ''", false)
	for _, kept := range []string{"'day'", "'123'", "''"} {
		if !strings.Contains(out, kept) {
			t.Errorf("safe literal %s must be kept: %s", kept, out)
		}
	}
}

func TestIdentifierWords(t *testing.T) {
	_, rw := fixture(t)
	words := rw.IdentifierWords(`SELECT toDate(created_at) FROM shop.orders WHERE x = 'lit'`)
	want := map[string]bool{"created_at": true, "shop": true, "orders": true, "x": true}
	for _, w := range words {
		if !want[w] {
			t.Errorf("unexpected identifier word %q", w)
		}
		delete(want, w)
	}
	for w := range want {
		t.Errorf("missing identifier word %q", w)
	}
}

func TestUnqualifiedColumnProbeOrder(t *testing.T) {
	im, rw := fixture(t)
	// "orders" appears as both nothing else; after a dot it must resolve as col first
	out := rw.Rewrite("SELECT o.created_at FROM shop.orders AS o", false)
	if !strings.Contains(out, mustTok(t, im, "col", "created_at")) {
		t.Errorf("member access must probe col first: %s", out)
	}
	if !strings.Contains(out, mustTok(t, im, "db", "shop")) || !strings.Contains(out, mustTok(t, im, "tbl", "orders")) {
		t.Errorf("qualified name must resolve db.tbl: %s", out)
	}
}
