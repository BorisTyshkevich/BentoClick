// Package chclient talks to ClickHouse by shelling out to an arbitrary
// command prefix that accepts clickhouse-client flags — either the binary
// itself (`clickhouse-client --connection demo`) or a wrapper that forwards
// flags to a remote cluster (`cl otel`, a kubectl-exec shim). Credentials
// stay in the prefix command's own config; the password never appears on
// argv. The Executor interface is kept narrow so a native driver
// (clickhouse-go) can replace it at MCP-integration time.
package chclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// LogComment tags every query this tool issues (via --log_comment) so that
// workload mining on the source cluster can exclude the tool's own footprint:
// WHERE log_comment != 'anond'. Without this, anond's profiling queries would
// pollute the very query_log it is trying to learn from.
const LogComment = "anond"

// stderrMax bounds captured stderr so a chatty wrapper or a huge ClickHouse
// stack trace cannot balloon memory; the head of stderr carries the error.
const stderrMax = 64 << 10

// Rows is a fully materialized TSVWithNamesAndTypes result. A nil cell is a
// ClickHouse NULL (\N). Result sets in this tool are bounded (profile queries,
// LIMITed samples), so in-memory is fine for v1. Bulk table data goes through
// QueryStream/InsertStream instead.
type Rows struct {
	Names []string
	Types []string
	Data  [][]*string
}

// Col returns the index of a named column, or -1.
func (r *Rows) Col(name string) int {
	for i, n := range r.Names {
		if n == name {
			return i
		}
	}
	return -1
}

type Executor interface {
	// Query runs sql and returns the result as TSVWithNamesAndTypes.
	Query(ctx context.Context, sql string) (*Rows, error)
	// Exec runs sql (DDL / INSERT...SELECT) and discards any output.
	Exec(ctx context.Context, sql string) error
	// Insert sends materialized rows into table via INSERT ... FORMAT TSV.
	Insert(ctx context.Context, table string, names []string, rows [][]*string) error
	// QueryStream runs sql with FORMAT TSV and streams stdout without
	// buffering. The caller must Close the reader to reap the process.
	QueryStream(ctx context.Context, sql string) (io.ReadCloser, error)
	// InsertStream pipes r into INSERT INTO table [(names)] FORMAT TSV.
	// Empty names means a positional insert (no column list).
	InsertStream(ctx context.Context, table string, names []string, r io.Reader) error
}

// Client executes through an argv prefix (binary + fixed flags).
type Client struct {
	// Prefix is the command and any fixed arguments, e.g.
	// ["clickhouse-client", "--connection", "demo"] or ["cl", "otel"].
	// Per-call flags (--query, --format, --log_comment) are appended.
	Prefix []string

	// RetryDelay is the pause before the single transient retry.
	// Zero means the 2s default; tests shrink it.
	RetryDelay time.Duration
}

// New builds a Client from an argv prefix.
func New(prefix []string) *Client {
	return &Client{Prefix: prefix}
}

// NewFromString whitespace-splits a command line, e.g. "cl otel" or
// "clickhouse-client --connection demo". Fields-splitting (no shell quoting)
// is deliberate: prefixes are operator-supplied flag vectors, not shell
// scripts, and avoiding a shell keeps SQL out of quoting trouble.
func NewFromString(cmd string) *Client {
	return New(strings.Fields(cmd))
}

// NewConnection is a convenience for the common local case of a named
// clickhouse-client connection.
func NewConnection(name string) *Client {
	return New([]string{"clickhouse-client", "--connection", name})
}

// argv assembles the full command line: prefix, the self-footprint tag, then
// the per-call flags. --log_comment goes on every invocation, no exceptions —
// see LogComment.
func (c *Client) argv(extra ...string) []string {
	a := make([]string, 0, len(c.Prefix)+2+len(extra))
	a = append(a, c.Prefix...)
	a = append(a, "--log_comment", LogComment)
	return append(a, extra...)
}

func (c *Client) command(ctx context.Context, extra ...string) (*exec.Cmd, error) {
	if len(c.Prefix) == 0 {
		return nil, fmt.Errorf("chclient: empty command prefix")
	}
	argv := c.argv(extra...)
	return exec.CommandContext(ctx, argv[0], argv[1:]...), nil
}

// transient reports whether stderr looks like a retriable network hiccup.
func transient(stderr string) bool {
	for _, s := range []string{
		"Timeout exceeded", "Connection refused", "Connection reset",
		"Broken pipe", "NETWORK_ERROR", "SOCKET_TIMEOUT", "ALL_CONNECTION_TRIES_FAILED",
		"unexpectedly closed",
	} {
		if strings.Contains(stderr, s) {
			return true
		}
	}
	return false
}

func (c *Client) runOnce(ctx context.Context, stdin []byte, extra ...string) ([]byte, error) {
	cmd, err := c.command(ctx, extra...)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	errb := &boundedBuffer{max: stderrMax}
	cmd.Stdout = &out
	cmd.Stderr = errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %s: %w", c.Prefix[0], strings.TrimSpace(errb.String()), err)
	}
	return out.Bytes(), nil
}

// run executes with a single retry on transient errors. Safe here because the
// whole stdin payload and the whole stdout are buffered, so a retry replays
// the exact same request.
func (c *Client) run(ctx context.Context, stdin []byte, extra ...string) ([]byte, error) {
	out, err := c.runOnce(ctx, stdin, extra...)
	if err != nil && transient(err.Error()) {
		delay := c.RetryDelay
		if delay == 0 {
			delay = 2 * time.Second
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(delay):
		}
		out, err = c.runOnce(ctx, stdin, extra...)
	}
	return out, err
}

func (c *Client) Query(ctx context.Context, sql string) (*Rows, error) {
	out, err := c.run(ctx, nil, "--query", sql, "--format", "TSVWithNamesAndTypes")
	if err != nil {
		return nil, err
	}
	return ParseNT(out)
}

func (c *Client) Exec(ctx context.Context, sql string) error {
	_, err := c.run(ctx, nil, "--query", sql)
	return err
}

func (c *Client) Insert(ctx context.Context, table string, names []string, rows [][]*string) error {
	if len(rows) == 0 {
		return nil
	}
	var b bytes.Buffer
	for _, row := range rows {
		WriteTSVRow(&b, row)
	}
	_, err := c.run(ctx, b.Bytes(), "--query", insertSQL(table, names))
	return err
}

// insertSQL builds the INSERT head; empty names means positional insert.
func insertSQL(table string, names []string) string {
	if len(names) == 0 {
		return fmt.Sprintf("INSERT INTO %s FORMAT TSV", table)
	}
	return fmt.Sprintf("INSERT INTO %s (%s) FORMAT TSV", table, strings.Join(names, ", "))
}

// QueryStream runs sql with FORMAT TSV and returns the child's stdout as a
// stream, for table data too large to buffer. The returned ReadCloser must be
// closed to reap the process; a non-zero exit surfaces (with stderr text)
// either from the Read that would have returned io.EOF or from Close.
//
// Streams are deliberately NOT retried on transient errors: by the time a
// failure is observed the caller may have consumed (and acted on) part of the
// output, so replaying from the start would duplicate data. Callers that need
// retries must restart the whole operation themselves.
func (c *Client) QueryStream(ctx context.Context, sql string) (io.ReadCloser, error) {
	cmd, err := c.command(ctx, "--query", sql, "--format", "TSV")
	if err != nil {
		return nil, err
	}
	errb := &boundedBuffer{max: stderrMax}
	cmd.Stderr = errb
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", c.Prefix[0], err)
	}
	return &procReader{rc: stdout, cmd: cmd, errb: errb, name: c.Prefix[0]}, nil
}

// InsertStream pipes r as stdin into INSERT INTO table [(names)] FORMAT TSV.
// Empty names omits the column list (positional insert). Not retried: r may
// have been partially consumed by the failed attempt, so a second run would
// insert a truncated or duplicated payload (see QueryStream).
func (c *Client) InsertStream(ctx context.Context, table string, names []string, r io.Reader) error {
	cmd, err := c.command(ctx, "--query", insertSQL(table, names))
	if err != nil {
		return err
	}
	errb := &boundedBuffer{max: stderrMax}
	cmd.Stderr = errb
	cmd.Stdin = r
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %s: %w", c.Prefix[0], strings.TrimSpace(errb.String()), err)
	}
	return nil
}

// procReader adapts a child process's stdout pipe into a ReadCloser whose
// Close (and the Read hitting end-of-stream) reaps the process and converts a
// non-zero exit into an error carrying the captured stderr.
type procReader struct {
	rc   io.ReadCloser
	cmd  *exec.Cmd
	errb *boundedBuffer
	name string

	mu      sync.Mutex
	waited  bool
	waitErr error
}

// wait reaps the child exactly once and caches the (stderr-annotated) result
// so both Read-at-EOF and Close report the same error.
func (p *procReader) wait() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.waited {
		p.waited = true
		if err := p.cmd.Wait(); err != nil {
			p.waitErr = fmt.Errorf("%s: %s: %w", p.name, strings.TrimSpace(p.errb.String()), err)
		}
	}
	return p.waitErr
}

func (p *procReader) Read(b []byte) (int, error) {
	n, err := p.rc.Read(b)
	if err == io.EOF {
		// The pipe drained: reap now so a failed query is reported as an
		// error instead of a silently truncated result.
		if werr := p.wait(); werr != nil {
			return n, werr
		}
	}
	return n, err
}

func (p *procReader) Close() error {
	// Closing the pipe first unblocks a child still writing (it gets EPIPE),
	// so Close cannot hang on an abandoned stream.
	p.rc.Close()
	return p.wait()
}

// boundedBuffer keeps at most max bytes and silently discards the rest; it
// never errors so the child is not killed by stderr backpressure.
type boundedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if remain := b.max - b.buf.Len(); remain > 0 {
		if len(p) > remain {
			p = p[:remain]
		}
		b.buf.Write(p)
	}
	return n, nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }
