package chclient

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var _ Executor = (*Client)(nil)

// writeScript drops a small /bin/sh script into a temp dir and returns its
// path. Tests use scripts as a fake "clickhouse-client" so no network or real
// binary is needed: the script records its argv/stdin and emits canned output.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-ch")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// recorderScript records argv (one arg per line) and stdin to files, then
// prints a minimal valid TSVWithNamesAndTypes payload so Query can parse it.
func recorderScript(t *testing.T) (script, argsFile, stdinFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	stdinFile = filepath.Join(dir, "stdin")
	script = writeScript(t, fmt.Sprintf(
		"printf '%%s\\n' \"$@\" > %q\ncat > %q\nprintf 'a\\nString\\nx\\n'\n",
		argsFile, stdinFile))
	return script, argsFile, stdinFile
}

func readArgs(t *testing.T, argsFile string) []string {
	t.Helper()
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// assertTagged checks the argv contains the --log_comment anond pair that
// lets source-cluster workload mining exclude anond's own queries.
func assertTagged(t *testing.T, args []string) {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--log_comment" && args[i+1] == LogComment {
			return
		}
	}
	t.Errorf("argv missing --log_comment %s: %v", LogComment, args)
}

func TestConstructors(t *testing.T) {
	if got := NewFromString("cl  mycluster").Prefix; len(got) != 2 || got[0] != "cl" || got[1] != "mycluster" {
		t.Errorf("NewFromString: %v", got)
	}
	want := []string{"clickhouse-client", "--connection", "demo"}
	got := NewConnection("demo").Prefix
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("NewConnection: %v", got)
	}
}

func TestEmptyPrefix(t *testing.T) {
	if err := New(nil).Exec(context.Background(), "SELECT 1"); err == nil {
		t.Error("expected error for empty prefix")
	}
}

// TestLogCommentEverywhere drives every Executor method against the recorder
// script and asserts each invocation is self-tagged.
func TestLogCommentEverywhere(t *testing.T) {
	ctx := context.Background()
	calls := map[string]func(c *Client) error{
		"Query": func(c *Client) error { _, err := c.Query(ctx, "SELECT 1"); return err },
		"Exec":  func(c *Client) error { return c.Exec(ctx, "SELECT 1") },
		"Insert": func(c *Client) error {
			return c.Insert(ctx, "t", []string{"a"}, [][]*string{{S("x")}})
		},
		"QueryStream": func(c *Client) error {
			rc, err := c.QueryStream(ctx, "SELECT 1")
			if err != nil {
				return err
			}
			if _, err := io.ReadAll(rc); err != nil {
				return err
			}
			return rc.Close()
		},
		"InsertStream": func(c *Client) error {
			return c.InsertStream(ctx, "t", []string{"a"}, strings.NewReader("x\n"))
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			script, argsFile, _ := recorderScript(t)
			// Extra prefix arg verifies the prefix is passed through verbatim.
			c := New([]string{script, "--wrapper-flag"})
			if err := call(c); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			args := readArgs(t, argsFile)
			if args[0] != "--wrapper-flag" {
				t.Errorf("prefix args not preserved: %v", args)
			}
			assertTagged(t, args)
		})
	}
}

func TestQueryStreamOutput(t *testing.T) {
	script := writeScript(t, "printf 'hello\\nworld\\n'\n")
	rc, err := New([]string{script}).QueryStream(context.Background(), "SELECT s FROM t")
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(out) != "hello\nworld\n" {
		t.Errorf("stdout = %q", out)
	}
	if err := rc.Close(); err != nil {
		t.Errorf("close after clean exit: %v", err)
	}
}

func TestQueryStreamFailure(t *testing.T) {
	script := writeScript(t, "echo 'boom: table gone' >&2\nexit 3\n")
	rc, err := New([]string{script}).QueryStream(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.ReadAll(rc)
	closeErr := rc.Close()
	// The failure must surface somewhere the caller cannot miss: the EOF-side
	// Read, Close, or both.
	if readErr == nil && closeErr == nil {
		t.Fatal("non-zero exit not surfaced by Read or Close")
	}
	for _, err := range []error{readErr, closeErr} {
		if err != nil && !strings.Contains(err.Error(), "boom") {
			t.Errorf("error missing stderr text: %v", err)
		}
	}
}

func TestInsertStreamPipesStdin(t *testing.T) {
	script, argsFile, stdinFile := recorderScript(t)
	payload := "a\tb\nc\td\n"
	err := New([]string{script}).InsertStream(context.Background(),
		"db.t", []string{"x", "y"}, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Errorf("stdin = %q, want %q", got, payload)
	}
	query := strings.Join(readArgs(t, argsFile), "\n")
	if !strings.Contains(query, "INSERT INTO db.t (x, y) FORMAT TSV") {
		t.Errorf("query missing column list: %q", query)
	}
}

func TestInsertStreamPositional(t *testing.T) {
	script, argsFile, _ := recorderScript(t)
	err := New([]string{script}).InsertStream(context.Background(),
		"db.t", nil, strings.NewReader("x\n"))
	if err != nil {
		t.Fatal(err)
	}
	query := strings.Join(readArgs(t, argsFile), "\n")
	if !strings.Contains(query, "INSERT INTO db.t FORMAT TSV") || strings.Contains(query, "(") {
		t.Errorf("positional insert built wrong query: %q", query)
	}
}

func TestInsertStreamFailure(t *testing.T) {
	script := writeScript(t, "cat > /dev/null\necho 'no such table' >&2\nexit 1\n")
	err := New([]string{script}).InsertStream(context.Background(),
		"db.t", nil, strings.NewReader("x\n"))
	if err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Errorf("want stderr-bearing error, got %v", err)
	}
}

// TestQueryRetryTransient fails the first run with a transient-looking stderr
// (tracked via a state file) and succeeds the second; Query must retry once.
func TestQueryRetryTransient(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	script := writeScript(t, fmt.Sprintf(
		"if [ ! -f %q ]; then : > %q; echo 'Code: 159. Timeout exceeded' >&2; exit 1; fi\nprintf 'a\\nString\\nx\\n'\n",
		state, state))
	c := &Client{Prefix: []string{script}, RetryDelay: time.Millisecond}
	rows, err := c.Query(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("retry did not recover: %v", err)
	}
	if len(rows.Data) != 1 || rows.Data[0][0] == nil || *rows.Data[0][0] != "x" {
		t.Errorf("unexpected rows after retry: %+v", rows)
	}
}

// TestQueryNoRetryOnPermanent: a non-transient failure must not be retried
// (the state file would have let a retry succeed).
func TestQueryNoRetryOnPermanent(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	script := writeScript(t, fmt.Sprintf(
		"if [ ! -f %q ]; then : > %q; echo 'Syntax error' >&2; exit 62; fi\nprintf 'a\\nString\\nx\\n'\n",
		state, state))
	c := &Client{Prefix: []string{script}, RetryDelay: time.Millisecond}
	if _, err := c.Query(context.Background(), "SELECT bogus("); err == nil {
		t.Error("permanent error was retried away")
	}
}
