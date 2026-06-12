package token

import (
	"strings"
	"testing"
)

func minter(t *testing.T) *Minter {
	m, err := NewMinter([]byte("test-key-0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDeterminism(t *testing.T) {
	m := minter(t)
	a := m.Token("tbl", "events", HexLen)
	b := m.Token("tbl", "events", HexLen)
	if a != b {
		t.Fatalf("same input, different tokens: %s vs %s", a, b)
	}
	if !strings.HasPrefix(a, "tbl_") || len(a) != len("tbl_")+HexLen {
		t.Fatalf("bad token shape: %s", a)
	}
}

func TestKindSeparation(t *testing.T) {
	m := minter(t)
	if m.Token("tbl", "events", HexLen)[4:] == m.Token("col", "events", HexLen)[4:] {
		t.Fatal("kinds must produce independent hash domains")
	}
}

func TestKeySeparation(t *testing.T) {
	m1 := minter(t)
	m2, _ := NewMinter([]byte("another-key-0123456789abcdef"))
	if m1.Token("tbl", "events", HexLen) == m2.Token("tbl", "events", HexLen) {
		t.Fatal("different keys must produce different tokens")
	}
}

func TestShortKeyRejected(t *testing.T) {
	if _, err := NewMinter([]byte("short")); err == nil {
		t.Fatal("short HMAC key must be rejected")
	}
}

func TestValueSeedIndependent(t *testing.T) {
	m := minter(t)
	if m.ValueSeed() == 0 {
		t.Fatal("seed must be non-zero (with overwhelming probability)")
	}
	if m.ValueSeed() != m.ValueSeed() {
		t.Fatal("seed must be deterministic")
	}
}

func TestReservedRe(t *testing.T) {
	m := minter(t)
	for _, kind := range Kinds {
		tok := m.Token(kind, "anything", HexLen)
		if !ReservedRe.MatchString(tok) {
			t.Errorf("minted token %s must match ReservedRe", tok)
		}
		wide := m.Token(kind, "anything", HexLenWide)
		if !ReservedRe.MatchString(wide) {
			t.Errorf("wide token %s must match ReservedRe", wide)
		}
	}
	for _, real := range []string{"events", "user_id", "db_main", "tbl_data", "col_x", "user_12345678x"} {
		if ReservedRe.MatchString(real) {
			t.Errorf("realistic identifier %q must NOT match ReservedRe", real)
		}
	}
	// the dangerous case: a real identifier that DOES look like a token
	if !ReservedRe.MatchString("tbl_deadbeef") {
		t.Error("tbl_deadbeef is inside the reserved namespace and must match (run aborts)")
	}
}
