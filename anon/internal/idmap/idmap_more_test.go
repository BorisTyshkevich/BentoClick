package idmap

// Additional tests to raise statement coverage from ~78% to ≥90%.
// Targets: KeepVerbatim("", …) early-return, Lookup (both found and not-found),
// Build with keep-only kinds, and Build 8-hex collision widening.
//
// Fixed key: "0123456789abcdef0123456789abcdef" (32 bytes).
// Collision pair precomputed by brute-force HMAC scan:
//   col_399d0ef3  ←  "val_0011904" and "val_0015648"
// Their 16-hex wide tokens differ, so this exercises the widening path
// without hitting the unresolvable-collision abort.

import (
	"strings"
	"testing"

	"github.com/Altinity/anon-discovery/internal/token"
)

// fixedMap returns an *IdMap backed by the deterministic 32-byte test key.
func fixedMap(t *testing.T) *IdMap {
	t.Helper()
	m, err := token.NewMinter([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return New(m)
}

// TestKeepVerbatimEmptyNoOp verifies that KeepVerbatim("", …) is a no-op
// (covers the early-return branch for empty value).
func TestKeepVerbatimEmptyNoOp(t *testing.T) {
	im := fixedMap(t)
	im.KeepVerbatim("db", "") // must not panic, must not add entry
	if err := im.Build(); err != nil {
		t.Fatal(err)
	}
	// The kind should not appear in the forward map at all.
	if _, ok := im.fwd["db"]; ok {
		t.Fatal("KeepVerbatim with empty value must not create a kind entry")
	}
}

// TestKeepVerbatimKindAlreadyExists adds two verbatim entries for the same
// kind so that the second call reaches the `im.keep[kind] != nil` branch
// (the map already exists — previously that path was never exercised).
func TestKeepVerbatimKindAlreadyExists(t *testing.T) {
	im := fixedMap(t)
	im.KeepVerbatim("db", "system")  // creates im.keep["db"]
	im.KeepVerbatim("db", "default") // reuses existing map — the gap branch
	if err := im.Build(); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"system", "default"} {
		tok, err := im.Map("db", v)
		if err != nil {
			t.Fatalf("Map(%q): %v", v, err)
		}
		if tok != v {
			t.Fatalf("verbatim %q must map to itself, got %q", v, tok)
		}
	}
}

// TestLookupFound verifies Lookup returns the token and true for a known value.
func TestLookupFound(t *testing.T) {
	im := fixedMap(t)
	im.Observe("tbl", "events")
	if err := im.Build(); err != nil {
		t.Fatal(err)
	}
	tok, ok := im.Lookup("tbl", "events")
	if !ok {
		t.Fatal("Lookup must return ok=true for an observed, built value")
	}
	if !strings.HasPrefix(tok, "tbl_") {
		t.Fatalf("unexpected token shape: %q", tok)
	}
}

// TestLookupNotFound verifies Lookup returns ("", false) for unknown values —
// fail-soft counterpart to Map's fail-closed behaviour.
func TestLookupNotFound(t *testing.T) {
	im := fixedMap(t)
	im.Observe("tbl", "events")
	if err := im.Build(); err != nil {
		t.Fatal(err)
	}
	tok, ok := im.Lookup("tbl", "ghost")
	if ok {
		t.Fatalf("Lookup of unobserved value must return ok=false; got token %q", tok)
	}
	if tok != "" {
		t.Fatalf("Lookup of unobserved value must return empty string; got %q", tok)
	}
}

// TestLookupUnknownKind verifies Lookup against a kind that was never even
// observed (im.fwd[kind] is nil).
func TestLookupUnknownKind(t *testing.T) {
	im := fixedMap(t)
	if err := im.Build(); err != nil {
		t.Fatal(err)
	}
	if tok, ok := im.Lookup("col", "anything"); ok || tok != "" {
		t.Fatalf("Lookup on unknown kind must return (\"\", false); got (%q, %v)", tok, ok)
	}
}

// TestBuildKeepOnlyKind exercises the "kinds that only have keep entries"
// branch at the bottom of Build (lines 95-103): a kind that has verbatim
// entries but zero Observe calls must still be present in the forward map.
func TestBuildKeepOnlyKind(t *testing.T) {
	im := fixedMap(t)
	// "db" has only keep entries; "tbl" has a real observed value.
	im.KeepVerbatim("db", "system")
	im.KeepVerbatim("db", "default")
	im.Observe("tbl", "orders")
	if err := im.Build(); err != nil {
		t.Fatal(err)
	}

	// Keep-only kind must map verbatim.
	for _, v := range []string{"system", "default"} {
		tok, err := im.Map("db", v)
		if err != nil {
			t.Fatalf("Map(db, %q): %v", v, err)
		}
		if tok != v {
			t.Fatalf("keep-only %q must map to itself; got %q", v, tok)
		}
	}

	// Normal kind still works.
	tok, err := im.Map("tbl", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "tbl_") {
		t.Fatalf("tbl/orders token shape wrong: %q", tok)
	}
}

// TestCollisionWidening exercises the 8-hex collision → 16-hex widening path.
//
// Precomputed with key "0123456789abcdef0123456789abcdef":
//
//	col_399d0ef3  ←  "val_0011904" and "val_0015648"
//
// Both widen to distinct 16-hex tokens, so the abort is NOT triggered — this
// covers Build lines 78-86 excluding the unresolvable-collision error return.
func TestCollisionWidening(t *testing.T) {
	const (
		v1 = "val_0011904"
		v2 = "val_0015648"
	)
	im := fixedMap(t)
	im.Observe("col", v1)
	im.Observe("col", v2)
	if err := im.Build(); err != nil {
		t.Fatalf("Build with 8-hex collision must succeed via widening: %v", err)
	}
	if im.Collisions != 1 {
		t.Fatalf("expected Collisions=1, got %d", im.Collisions)
	}

	// Both values must map to distinct 16-hex tokens.
	tok1, err := im.Map("col", v1)
	if err != nil {
		t.Fatalf("Map col/%s: %v", v1, err)
	}
	tok2, err := im.Map("col", v2)
	if err != nil {
		t.Fatalf("Map col/%s: %v", v2, err)
	}
	if tok1 == tok2 {
		t.Fatalf("widened tokens must be distinct: %s", tok1)
	}
	// Wide tokens are 16 hex chars after the kind_ prefix.
	for _, tok := range []string{tok1, tok2} {
		if !strings.HasPrefix(tok, "col_") {
			t.Fatalf("wide token shape wrong: %q", tok)
		}
		hex := strings.TrimPrefix(tok, "col_")
		if len(hex) != 16 {
			t.Fatalf("wide token should have 16 hex chars, got %d in %q", len(hex), tok)
		}
	}
}

// TestDeterminism verifies that the same (kind, value) under the same key
// always produces the same token across two independent builds.
func TestDeterminism(t *testing.T) {
	build := func() string {
		im := fixedMap(t)
		im.Observe("tbl", "sessions")
		if err := im.Build(); err != nil {
			t.Fatal(err)
		}
		tok, _ := im.Map("tbl", "sessions")
		return tok
	}
	if t1, t2 := build(), build(); t1 != t2 {
		t.Fatalf("token not deterministic: %q vs %q", t1, t2)
	}
}

// TestObserveBeforeBuildOrdering verifies fail-closed behaviour:
// Lookup on a value not observed before Build returns (_, false).
func TestObserveBeforeBuildOrdering(t *testing.T) {
	im := fixedMap(t)
	im.Observe("tbl", "observed_early")
	if err := im.Build(); err != nil {
		t.Fatal(err)
	}
	// Observed before build: must be found.
	if _, ok := im.Lookup("tbl", "observed_early"); !ok {
		t.Fatal("observed value must be found after Build")
	}
	// Never observed: must NOT be found.
	if _, ok := im.Lookup("tbl", "late_arrival"); ok {
		t.Fatal("unobserved value must not be present after Build")
	}
}
