package idmap

import (
	"strings"
	"testing"

	"github.com/Altinity/anon-discovery/internal/token"
)

func newMap(t *testing.T) *IdMap {
	m, err := token.NewMinter([]byte("test-key-0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return New(m)
}

func TestBijection(t *testing.T) {
	im := newMap(t)
	vals := []string{"events", "users", "orders", "line_items", "events_local"}
	for _, v := range vals {
		im.Observe("tbl", v)
	}
	if err := im.Build(); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, v := range vals {
		tok, err := im.Map("tbl", v)
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[tok]; dup {
			t.Fatalf("token %s assigned to both %q and %q", tok, prev, v)
		}
		seen[tok] = v
		if !strings.HasPrefix(tok, "tbl_") {
			t.Fatalf("bad token %s", tok)
		}
	}
}

func TestKeepVerbatim(t *testing.T) {
	im := newMap(t)
	im.KeepVerbatim("db", "system")
	im.Observe("db", "system") // observe after keep: still identity
	im.Observe("db", "mydata")
	if err := im.Build(); err != nil {
		t.Fatal(err)
	}
	tok, _ := im.Map("db", "system")
	if tok != "system" {
		t.Fatalf("system must map to itself, got %s", tok)
	}
	tok, _ = im.Map("db", "mydata")
	if tok == "mydata" {
		t.Fatal("mydata must be tokenized")
	}
}

func TestFailClosedOnUnobserved(t *testing.T) {
	im := newMap(t)
	im.Observe("tbl", "events")
	if err := im.Build(); err != nil {
		t.Fatal(err)
	}
	if _, err := im.Map("tbl", "never_seen"); err == nil {
		t.Fatal("unobserved identifier must error, not silently leak or mint")
	}
}

func TestReservedNamespaceAborts(t *testing.T) {
	im := newMap(t)
	im.Observe("tbl", "tbl_deadbeef") // a real table named like a token
	if err := im.Build(); err == nil {
		t.Fatal("identifier inside the reserved token namespace must abort the build")
	}
}

func TestEmptyValuesIgnored(t *testing.T) {
	im := newMap(t)
	im.Observe("col", "")
	if err := im.Build(); err != nil {
		t.Fatal(err)
	}
	if tok, err := im.Map("col", ""); err != nil || tok != "" {
		t.Fatalf("empty value must pass through: %q %v", tok, err)
	}
}

func TestKnown(t *testing.T) {
	im := newMap(t)
	im.Observe("tbl", "events")
	im.Observe("col", "user_id")
	im.KeepVerbatim("db", "system")
	known := im.Known([]string{"tbl", "col"})
	for _, want := range []string{"events", "user_id", "system"} {
		if !known[want] {
			t.Errorf("Known() must include %q", want)
		}
	}
}

func TestPairsSortedAndComplete(t *testing.T) {
	im := newMap(t)
	im.Observe("tbl", "b")
	im.Observe("tbl", "a")
	im.Observe("db", "z")
	if err := im.Build(); err != nil {
		t.Fatal(err)
	}
	p := im.Pairs()
	if len(p) != 3 {
		t.Fatalf("want 3 pairs, got %d", len(p))
	}
	if p[0][0] != "db" || p[1][1] != "a" || p[2][1] != "b" {
		t.Fatalf("pairs not sorted: %v", p)
	}
}
