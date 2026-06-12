// Package chclient talks to ClickHouse by shelling out to clickhouse-client.
// Credentials stay in the client's own config (--connection <name>); the
// password never appears on argv. The Executor interface is kept narrow so a
// native driver (clickhouse-go) can replace it at MCP-integration time.
package chclient

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Rows is a fully materialized TSVWithNamesAndTypes result. A nil cell is a
// ClickHouse NULL (\N). Result sets in this tool are bounded (profile queries,
// LIMITed samples), so in-memory is fine for v1.
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
	// Insert streams rows into table via INSERT ... FORMAT TSV on stdin.
	Insert(ctx context.Context, table string, names []string, rows [][]*string) error
}

// Client executes through the clickhouse-client binary.
type Client struct {
	Connection string // --connection name; empty = client defaults/env
	Bin        string // defaults to "clickhouse-client"
}

func New(connection string) *Client {
	return &Client{Connection: connection, Bin: "clickhouse-client"}
}

func (c *Client) args(extra ...string) []string {
	var a []string
	if c.Connection != "" {
		a = append(a, "--connection", c.Connection)
	}
	return append(a, extra...)
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
	cmd := exec.CommandContext(ctx, c.Bin, c.args(extra...)...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("clickhouse-client: %s: %w", strings.TrimSpace(errb.String()), err)
	}
	return out.Bytes(), nil
}

// run executes with a single retry on transient errors.
func (c *Client) run(ctx context.Context, stdin []byte, extra ...string) ([]byte, error) {
	out, err := c.runOnce(ctx, stdin, extra...)
	if err != nil && transient(err.Error()) {
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(2 * time.Second):
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
	sql := fmt.Sprintf("INSERT INTO %s (%s) FORMAT TSV", table, strings.Join(names, ", "))
	_, err := c.run(ctx, b.Bytes(), "--query", sql)
	return err
}
