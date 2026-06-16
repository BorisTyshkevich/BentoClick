package chclient

import (
	"bytes"
	"testing"
)

func TestParseNT(t *testing.T) {
	payload := []byte("name\ttype_col\nString\tNullable(String)\n" +
		"plain\tvalue\n" +
		"with\\ttab\t\\N\n" +
		"multi\\nline\tx\\\\y\n")
	rows, err := ParseNT(payload)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Names[0] != "name" || rows.Types[1] != "Nullable(String)" {
		t.Fatalf("header: %v %v", rows.Names, rows.Types)
	}
	if len(rows.Data) != 3 {
		t.Fatalf("rows: %d", len(rows.Data))
	}
	if *rows.Data[1][0] != "with\ttab" {
		t.Errorf("escaped tab: %q", *rows.Data[1][0])
	}
	if rows.Data[1][1] != nil {
		t.Errorf("\\N must parse as NULL")
	}
	if *rows.Data[2][0] != "multi\nline" || *rows.Data[2][1] != `x\y` {
		t.Errorf("escapes: %q %q", *rows.Data[2][0], *rows.Data[2][1])
	}
}

func TestRoundTrip(t *testing.T) {
	vals := []string{"plain", "tab\there", "nl\nthere", `back\slash`, "zero\x00byte", ""}
	var b bytes.Buffer
	row := make([]*string, len(vals)+1)
	for i := range vals {
		row[i] = &vals[i]
	}
	row[len(vals)] = nil
	WriteTSVRow(&b, row)

	payload := append([]byte("a\tb\tc\td\te\tf\tg\nString\tString\tString\tString\tString\tString\tNullable(String)\n"), b.Bytes()...)
	rows, err := ParseNT(payload)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range vals {
		if *rows.Data[0][i] != want {
			t.Errorf("col %d: %q != %q", i, *rows.Data[0][i], want)
		}
	}
	if rows.Data[0][len(vals)] != nil {
		t.Error("nil cell must round-trip as NULL")
	}
}

func TestParseNTErrors(t *testing.T) {
	if _, err := ParseNT([]byte("only-names\n")); err == nil {
		t.Error("missing types header must error")
	}
	if _, err := ParseNT([]byte("a\tb\nString\tString\nonly-one-cell\n")); err == nil {
		t.Error("row width mismatch must error")
	}
}

func TestTransientDetection(t *testing.T) {
	if !transient("Code: 209. DB::NetException: Timeout exceeded while reading") {
		t.Error("timeout must be transient")
	}
	if transient("Code: 60. DB::Exception: Table x doesn't exist") {
		t.Error("missing table must not be transient")
	}
}
