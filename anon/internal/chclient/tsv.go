package chclient

import (
	"bytes"
	"fmt"
	"strings"
)

// TSV escaping per ClickHouse TabSeparated: \b \f \r \n \t \0 \' \\ and \N
// for NULL. Real tabs/newlines are always separators (in-field ones arrive as
// two-byte escape sequences), so raw split-then-unescape is exact.
// Port of the collector's tsv.py rules.

// ParseNT parses a TSVWithNamesAndTypes payload.
func ParseNT(b []byte) (*Rows, error) {
	r := &Rows{}
	lines := splitLines(b)
	if len(lines) < 2 {
		return nil, fmt.Errorf("tsv: result has no names+types header (%d lines)", len(lines))
	}
	r.Names = header(lines[0])
	r.Types = header(lines[1])
	if len(r.Names) != len(r.Types) {
		return nil, fmt.Errorf("tsv: names/types width mismatch (%d vs %d)", len(r.Names), len(r.Types))
	}
	for _, line := range lines[2:] {
		fields := bytes.Split(line, []byte{'\t'})
		if len(fields) != len(r.Names) {
			return nil, fmt.Errorf("tsv: row width %d, want %d: %q", len(fields), len(r.Names), string(line))
		}
		row := make([]*string, len(fields))
		for i, f := range fields {
			if len(f) == 2 && f[0] == '\\' && f[1] == 'N' {
				continue // NULL
			}
			v := Unescape(string(f))
			row[i] = &v
		}
		r.Data = append(r.Data, row)
	}
	return r, nil
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			out = append(out, b)
			break
		}
		out = append(out, b[:i])
		b = b[i+1:]
	}
	for len(out) > 0 && len(out[len(out)-1]) == 0 {
		out = out[:len(out)-1]
	}
	return out
}

// header fields use the same escaping but are never NULL.
func header(line []byte) []string {
	fields := bytes.Split(line, []byte{'\t'})
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = Unescape(string(f))
	}
	return out
}

// Unescape decodes ClickHouse TSV escapes in one field.
func Unescape(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch s[i] {
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'r':
			b.WriteByte('\r')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case '0':
			b.WriteByte(0)
		case '\'':
			b.WriteByte('\'')
		case '\\':
			b.WriteByte('\\')
		default: // unknown escape: keep verbatim
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// EscapeTSV escapes one value for a TabSeparated cell.
func EscapeTSV(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case 0:
			b.WriteString(`\0`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// WriteTSVRow appends one escaped TSV line; nil cells become \N.
func WriteTSVRow(b *bytes.Buffer, row []*string) {
	for i, c := range row {
		if i > 0 {
			b.WriteByte('\t')
		}
		if c == nil {
			b.WriteString(`\N`)
		} else {
			b.WriteString(EscapeTSV(*c))
		}
	}
	b.WriteByte('\n')
}

// S is a convenience for building literal cells.
func S(s string) *string { return &s }
