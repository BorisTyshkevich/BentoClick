package integration

// Embedded ClickHouse bootstrap for the integration suite — no Docker.
//
// If a connection is already provided (ANON_TEST_CMD / ANON_TEST_CONNECTION,
// or the cross-cluster ANON_TEST_SOURCE_CMD), the suite uses it unchanged.
// Otherwise, when a clickhouse multicall binary is available, TestMain boots an
// embedded server via franchb/embedded-clickhouse and points ANON_TEST_CMD at
// it — the SAME binary serves as the client (`clickhouse client --host ...`).
// With no connection and no binary, the suite skips as before.
//
// Binary resolution: $ANON_TEST_CH_BINARY, else `clickhouse` (the multicall
// binary) on PATH. embedded-clickhouse runs it as the server (BinaryPath), so
// no image pull / GitHub-release download is needed — CI installs it once.

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	embeddedch "github.com/franchb/embedded-clickhouse"
)

func TestMain(m *testing.M) {
	// A connection is already configured — run as-is.
	if os.Getenv("ANON_TEST_CMD") != "" ||
		os.Getenv("ANON_TEST_CONNECTION") != "" ||
		os.Getenv("ANON_TEST_SOURCE_CMD") != "" {
		os.Exit(m.Run())
	}

	bin := resolveCHBinary()
	if bin == "" {
		// No connection and no binary: the suite's t.Skip paths handle it.
		os.Exit(m.Run())
	}

	ch := embeddedch.NewServer(
		embeddedch.DefaultConfig().
			Version(embeddedch.V26_3).
			BinaryPath(bin).
			StartTimeout(90 * time.Second),
	)
	if err := ch.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "embedded clickhouse start failed: %v\n", err)
		os.Exit(1)
	}
	host, port, err := net.SplitHostPort(ch.TCPAddr())
	if err != nil {
		fmt.Fprintf(os.Stderr, "embedded clickhouse TCP addr %q: %v\n", ch.TCPAddr(), err)
		_ = ch.Stop()
		os.Exit(1)
	}
	// Reuse the same multicall binary as the client; the embedded server's
	// default user is passwordless plaintext on localhost. Point the client at
	// an empty config so it ignores any host ~/.clickhouse-client config (whose
	// connections_credentials may default to TLS) + --no-secure for good measure.
	emptyCfg := filepath.Join(os.TempDir(), "anon-ch-client-config.xml")
	if err := os.WriteFile(emptyCfg, []byte("<clickhouse/>\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write empty client config: %v\n", err)
		_ = ch.Stop()
		os.Exit(1)
	}
	// Strip any inherited cluster credentials (the sandbox/cl wrapper exports
	// CLICKHOUSE_USER=<email> etc.) so the client subprocess connects as the
	// embedded server's passwordless `default` user instead.
	for _, k := range []string{"CLICKHOUSE_USER", "CLICKHOUSE_PASSWORD", "CLICKHOUSE_DATABASE", "CLICKHOUSE_HOST", "CLICKHOUSE_PORT"} {
		os.Unsetenv(k)
	}
	// --config-file empty: ignore any host ~/.clickhouse-client config (whose
	// connections_credentials may default to TLS). --no-secure + --user default
	// pin a plaintext connection as the passwordless default user.
	os.Setenv("ANON_TEST_CMD", fmt.Sprintf(
		"%s client --config-file %s --host %s --port %s --no-secure --user default",
		bin, emptyCfg, host, port))

	code := m.Run()
	_ = ch.Stop()
	os.Exit(code)
}

// resolveCHBinary returns the clickhouse MULTICALL binary path, or "" if none.
// Must be `clickhouse` (serves AND clients); `clickhouse-client` is client-only
// and can't run the embedded server.
func resolveCHBinary() string {
	if b := os.Getenv("ANON_TEST_CH_BINARY"); b != "" {
		return b
	}
	if p, err := exec.LookPath("clickhouse"); err == nil {
		return p
	}
	return ""
}
