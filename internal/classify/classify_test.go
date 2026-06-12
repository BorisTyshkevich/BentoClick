package classify

import (
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
		{Column{Name: "attrs", Type: "Map(String, String)"}, ClassSchemaless},
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
	expr, outType, inc := MaskExpr(Column{Name: "user_id", Type: "UInt64"}, ClassJoinKey, seed)
	if !inc || outType != "UInt64" || !strings.Contains(expr, "sipHash64") {
		t.Errorf("numeric joinkey: %s %s %v", expr, outType, inc)
	}

	// string join key -> hex string
	expr, outType, _ = MaskExpr(Column{Name: "tenant", Type: "String"}, ClassJoinKey, seed)
	if outType != "String" || !strings.Contains(expr, "hex(") {
		t.Errorf("string joinkey: %s %s", expr, outType)
	}

	// nullable preserved
	expr, outType, _ = MaskExpr(Column{Name: "ref_id", Type: "Nullable(String)"}, ClassJoinKey, seed)
	if outType != "Nullable(String)" || !strings.Contains(expr, "isNull") {
		t.Errorf("nullable joinkey: %s %s", expr, outType)
	}

	// label keeps LowCardinality nesting order
	_, outType, _ = MaskExpr(Column{Name: "country", Type: "LowCardinality(Nullable(String))"}, ClassLabel, seed)
	if outType != "LowCardinality(Nullable(String))" {
		t.Errorf("label type: %s", outType)
	}

	// freetext -> constant
	expr, _, _ = MaskExpr(Column{Name: "comment", Type: "String"}, ClassFreeText, seed)
	if expr != "'[redacted]'" {
		t.Errorf("freetext expr: %s", expr)
	}

	// schemaless excluded
	if _, _, inc := MaskExpr(Column{Name: "payload", Type: "JSON"}, ClassSchemaless, seed); inc {
		t.Error("schemaless must be excluded")
	}

	// kept classes pass through with original type
	expr, outType, _ = MaskExpr(Column{Name: "event_time", Type: "DateTime"}, ClassTime, seed)
	if expr != "`event_time`" || outType != "DateTime" {
		t.Errorf("time keep: %s %s", expr, outType)
	}
}

func TestSeedAppearsOnlyInMaskedExprs(t *testing.T) {
	const seed = uint64(123456789)
	expr, _, _ := MaskExpr(Column{Name: "event_time", Type: "DateTime"}, ClassTime, seed)
	if strings.Contains(expr, "123456789") {
		t.Error("kept columns must not embed the seed")
	}
	expr, _, _ = MaskExpr(Column{Name: "user_id", Type: "UInt64"}, ClassJoinKey, seed)
	if !strings.Contains(expr, "123456789") {
		t.Error("hash expressions embed the seed (trusted-side only)")
	}
}
