package classify

import (
	"regexp"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		col  Column
		want Class
	}{
		{Column{Name: "event_time", Type: "DateTime"}, ClassTime},
		{Column{Name: "d", Type: "Date"}, ClassTime},
		{Column{Name: "ts", Type: "Nullable(DateTime64(3))"}, ClassTime},
		{Column{Name: "amount", Type: "Float64"}, ClassMeasure},
		{Column{Name: "qty", Type: "UInt32"}, ClassMeasure},
		{Column{Name: "price", Type: "Decimal(18, 2)"}, ClassMeasure},
		{Column{Name: "status", Type: "Enum8('a' = 1, 'b' = 2)"}, ClassEnum},
		{Column{Name: "user_id", Type: "UInt64"}, ClassJoinKey},
		{Column{Name: "id", Type: "String"}, ClassJoinKey},
		{Column{Name: "session_uuid", Type: "UUID"}, ClassJoinKey},
		{Column{Name: "ip", Type: "IPv4"}, ClassJoinKey},
		{Column{Name: "tenant", Type: "String", InSK: true}, ClassJoinKey},
		{Column{Name: "country", Type: "LowCardinality(String)"}, ClassLabel},
		{Column{Name: "comment", Type: "String"}, ClassFreeText},
		{Column{Name: "payload", Type: "JSON"}, ClassSchemaless},
		{Column{Name: "attrs", Type: "Map(String, String)"}, ClassAttrMap},
		{Column{Name: "ResourceAttributes", Type: "Map(LowCardinality(String), String)"}, ClassAttrMap},
		// pure-value map: KEY is String but values aren't maskable per-value — fail closed in v1
		{Column{Name: "counts", Type: "Map(String, UInt64)"}, ClassSchemaless},
		{Column{Name: "by_id", Type: "Map(UInt64, String)"}, ClassSchemaless},
		{Column{Name: "opt_attrs", Type: "Map(String, Nullable(String))"}, ClassSchemaless},
		{Column{Name: "lc_attrs", Type: "Map(String, LowCardinality(String))"}, ClassSchemaless},
		{Column{Name: "tags", Type: "Array(String)"}, ClassSchemaless},
		{Column{Name: "vals", Type: "Array(UInt64)"}, ClassMeasure}, // pure-value complex: keep
		{Column{Name: "dyn", Type: "Dynamic"}, ClassSchemaless},
		{Column{Name: "var", Type: "Variant(UInt64, String)"}, ClassSchemaless},
	}
	for _, c := range cases {
		if got := Classify(c.col); got != c.want {
			t.Errorf("Classify(%s %s) = %s, want %s", c.col.Name, c.col.Type, got, c.want)
		}
	}
}

func TestMaskExprShapes(t *testing.T) {
	const seed = uint64(0xDEADBEEF)

	// numeric join key -> UInt64 hash
	expr, outType, inc := MaskExpr(Column{Name: "user_id", Type: "UInt64"}, ClassJoinKey, seed, nil)
	if !inc || outType != "UInt64" || !strings.Contains(expr, "sipHash64") {
		t.Errorf("numeric joinkey: %s %s %v", expr, outType, inc)
	}

	// string join key -> hex string
	expr, outType, _ = MaskExpr(Column{Name: "tenant", Type: "String"}, ClassJoinKey, seed, nil)
	if outType != "String" || !strings.Contains(expr, "hex(") {
		t.Errorf("string joinkey: %s %s", expr, outType)
	}

	// nullable preserved
	expr, outType, _ = MaskExpr(Column{Name: "ref_id", Type: "Nullable(String)"}, ClassJoinKey, seed, nil)
	if outType != "Nullable(String)" || !strings.Contains(expr, "isNull") {
		t.Errorf("nullable joinkey: %s %s", expr, outType)
	}

	// label keeps LowCardinality nesting order
	_, outType, _ = MaskExpr(Column{Name: "country", Type: "LowCardinality(Nullable(String))"}, ClassLabel, seed, nil)
	if outType != "LowCardinality(Nullable(String))" {
		t.Errorf("label type: %s", outType)
	}

	// freetext -> constant
	expr, _, _ = MaskExpr(Column{Name: "comment", Type: "String"}, ClassFreeText, seed, nil)
	if expr != "'[redacted]'" {
		t.Errorf("freetext expr: %s", expr)
	}

	// attrmap shape: map rebuilt from token-keys + masked-values, K type preserved
	expr, outType, inc = MaskExpr(
		Column{Name: "ResourceAttributes", Type: "Map(LowCardinality(String), String)"}, ClassAttrMap, seed, nil)
	if !inc {
		t.Error("attrmap must be included")
	}
	if outType != "Map(LowCardinality(String), String)" {
		t.Errorf("attrmap type: %s", outType)
	}
	for _, frag := range []string{"mapFromArrays", "mapKeys", "arrayMap", "sipHash64(3735928559, v)"} {
		if !strings.Contains(expr, frag) {
			t.Errorf("attrmap expr missing %q: %s", frag, expr)
		}
	}

	// schemaless excluded
	if _, _, inc := MaskExpr(Column{Name: "payload", Type: "JSON"}, ClassSchemaless, seed, nil); inc {
		t.Error("schemaless must be excluded")
	}

	// kept classes pass through with original type
	expr, outType, _ = MaskExpr(Column{Name: "event_time", Type: "DateTime"}, ClassTime, seed, nil)
	if expr != "`event_time`" || outType != "DateTime" {
		t.Errorf("time keep: %s %s", expr, outType)
	}
}

// TestAttrMaskRoleGated (P1): a value is kept real ONLY for a vocabulary key, or
// a measure key whose value is numeric. identity/sensitive AND unknown keys are
// hashed even when numeric — the old unconditional numeric-keep is gone.
func TestAttrMaskRoleGated(t *testing.T) {
	const seed = uint64(7)
	col := Column{Name: "LogAttributes", Type: "Map(String, String)"}

	// nil/empty spec -> nothing kept (every value hashed): predicate is `if(0,`,
	// and the old unconditional numeric/bool keep must NOT be present.
	expr, _, _ := MaskExpr(col, ClassAttrMap, seed, &AttrMaskSpec{})
	if !strings.Contains(expr, "if(0, v,") {
		t.Errorf("empty spec must hash every value (if(0,...)): %s", expr)
	}
	if strings.Contains(expr, "v IN ('true', 'false')") || strings.Contains(expr, "^-?[0-9.]+$") {
		t.Errorf("unconditional numeric/bool keep must be gone: %s", expr)
	}

	// vocabulary key kept; measure key kept iff numeric; identity key NOT listed
	// (so it falls through to the hash).
	spec := &AttrMaskSpec{VocabKeys: []string{"event.name"}, MeasureKeys: []string{"output_tokens"}}
	expr, _, _ = MaskExpr(col, ClassAttrMap, seed, spec)
	if !strings.Contains(expr, "k IN ('event.name')") {
		t.Errorf("vocabulary keep missing: %s", expr)
	}
	if !strings.Contains(expr, "k IN ('output_tokens') AND match(v,") {
		t.Errorf("measure numeric-gated keep missing: %s", expr)
	}
	if strings.Contains(expr, "user.id") {
		t.Errorf("identity key must not appear in a keep clause: %s", expr)
	}
}

// TestAttrKeyTokenization (P3): semconv keys stay real; custom keys map to their
// field_<hex> token via transform(); unobserved non-semconv keys hit the sentinel.
func TestAttrKeyTokenization(t *testing.T) {
	if !IsSemconvKey("http.method") || !IsSemconvKey("service.name") || !IsSemconvKey("user.id") {
		t.Error("semconv keys must be recognized")
	}
	if IsSemconvKey("internal_codename") || IsSemconvKey("acme_feature.flag") {
		t.Error("custom keys must NOT be treated as semconv")
	}
	expr, _, _ := MaskExpr(
		Column{Name: "LogAttributes", Type: "Map(String, String)"}, ClassAttrMap, 7,
		&AttrMaskSpec{CustomKeys: []string{"internal_codename"}, CustomToks: []string{"field_abc123"}})
	if !strings.Contains(expr, "transform(k, ['internal_codename'], ['field_abc123']") {
		t.Errorf("custom key must be transformed to its token: %s", expr)
	}
	if !strings.Contains(expr, "'field_redacted'") {
		t.Errorf("unobserved non-semconv key must hit the sentinel: %s", expr)
	}
}

// TestClassifyAttrKeyIdentity (P1): a PII-named key is identity (masked) even when
// its values are numeric; a numeric measure key (not PII-named) stays measure.
func TestClassifyAttrKeyIdentity(t *testing.T) {
	deny := regexp.MustCompile(DefaultPIIKeyPattern)
	if r := ClassifyAttrKey("account.number", 100000, 1.0, 64, deny); r != RoleIdentity {
		t.Errorf("PII-named numeric key must be identity, got %s", r)
	}
	if r := ClassifyAttrKey("user.id", 100000, 1.0, 64, deny); r != RoleIdentity {
		t.Errorf("user.id must be identity, got %s", r)
	}
	if r := ClassifyAttrKey("output_tokens", 5000, 1.0, 64, deny); r != RoleMeasure {
		t.Errorf("numeric non-PII key must be measure, got %s", r)
	}
}

func TestSeedAppearsOnlyInMaskedExprs(t *testing.T) {
	const seed = uint64(123456789)
	expr, _, _ := MaskExpr(Column{Name: "event_time", Type: "DateTime"}, ClassTime, seed, nil)
	if strings.Contains(expr, "123456789") {
		t.Error("kept columns must not embed the seed")
	}
	expr, _, _ = MaskExpr(Column{Name: "user_id", Type: "UInt64"}, ClassJoinKey, seed, nil)
	if !strings.Contains(expr, "123456789") {
		t.Error("hash expressions embed the seed (trusted-side only)")
	}
}
