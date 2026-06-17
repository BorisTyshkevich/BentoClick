// anond — anonymized ClickHouse schema discovery (standalone experiment).
//
// Subcommands:
//
//	run      full pipeline: discover on the source, build map, write profile
//	         and materialize the sandbox on the dest
//	print    render profile sections from the latest complete run (dest)
//	verify   acceptance checks against the latest run (no-survivor on dest,
//	         sandbox leaks, trusted tables absent from the dest meta DB)
//	cleanup  drop ONLY objects this tool registered in the dest registry
//
// Cluster targeting: --source / --dest take whitespace-split command prefixes
// that accept clickhouse-client flags ("cl otel", "clickhouse-client
// --connection demo"). --connection <name> is sugar for source = dest =
// "clickhouse-client --connection <name>" (single-cluster mode). Omitting
// --dest means dest = source.
//
// The HMAC key comes from ANON_HMAC_KEY (or --hmac-key-file): it determines
// every token; keep it as secret as the data.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Altinity/anon-discovery/internal/chclient"
	"github.com/Altinity/anon-discovery/internal/discover"
	"github.com/Altinity/anon-discovery/internal/store"
	"github.com/Altinity/anon-discovery/internal/token"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "print":
		err = cmdPrint(args)
	case "verify":
		err = cmdVerify(args)
	case "cleanup":
		err = cmdCleanup(args)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "anond %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: anond <run|print|verify|cleanup> [flags]
  common  --source "<cmd>" [--dest "<cmd>"] [--meta-db altinity]
          (--connection <name> = sugar for source=dest="clickhouse-client --connection <name>")
  run     --source-db <db> [--dest-db <db>] [--window-days 7] [--sample-rows 1000000]
          [--service-users u1,u2] [--dry-run]
  print   [--section shape|catalog|workload|relations|conventions|verification|manifest]
  verify  (no extra flags)
  cleanup [--include-meta]
HMAC key: ANON_HMAC_KEY env var or --hmac-key-file <path> (run only).`)
}

type clusterFlags struct {
	conn, source, dest, metaDB *string
}

func commonFlags(fs *flag.FlagSet) clusterFlags {
	return clusterFlags{
		conn:   fs.String("connection", "", "clickhouse-client connection name (single-cluster sugar)"),
		source: fs.String("source", "", `source cluster command prefix, e.g. "cl otel"`),
		dest:   fs.String("dest", "", "dest (sandbox) cluster command prefix (default: same as source)"),
		metaDB: fs.String("meta-db", "bentoclick", "metadata database (profile_*, generated_objects, manifest)"),
	}
}

// resolve turns the flag triple into concrete source/dest command strings.
// Identical strings mean single-cluster mode (the trusted-split checks relax).
func (cf clusterFlags) resolve() (srcCmd, dstCmd string, err error) {
	srcCmd = *cf.source
	if srcCmd == "" && *cf.conn != "" {
		srcCmd = "clickhouse-client --connection " + *cf.conn
	}
	if srcCmd == "" {
		return "", "", fmt.Errorf("need --source (or --connection)")
	}
	dstCmd = *cf.dest
	if dstCmd == "" {
		dstCmd = srcCmd
	}
	return srcCmd, dstCmd, nil
}

func loadKey(keyFile string) ([]byte, error) {
	if keyFile != "" {
		b, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, err
		}
		return []byte(strings.TrimSpace(string(b))), nil
	}
	if k := os.Getenv("ANON_HMAC_KEY"); k != "" {
		return []byte(k), nil
	}
	return nil, fmt.Errorf("no HMAC key: set ANON_HMAC_KEY or pass --hmac-key-file")
}

func logf(start time.Time) func(string, ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "[%6.1fs] %s\n", time.Since(start).Seconds(), fmt.Sprintf(format, args...))
	}
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cf := commonFlags(fs)
	sourceDB := fs.String("source-db", "", "the single database to mirror (required)")
	destDB := fs.String("dest-db", "", "sandbox database name on the dest cluster (default: <source-db>_anon)")
	model := fs.String("model", "tokenizing", "anonymization model: tokenizing (token names, masked values) | schema-preserving (real names, masked values)")
	window := fs.Int("window-days", 7, "query_log mining window")
	sample := fs.Uint64("sample-rows", 1_000_000, "max sandbox rows per table")
	serviceUsers := fs.String("service-users", "", "comma-separated service users (demotion rule 2)")
	keyFile := fs.String("hmac-key-file", "", "file containing the HMAC key")
	keepAttrKeys := fs.String("keep-attr-keys", "", "comma-separated attrmap KEYS to force-keep verbatim (manual override on top of auto-classification)")
	attrCardThreshold := fs.Uint64("attr-card-threshold", 64, "max distinct values for an attrmap key to auto-classify as vocabulary (value kept real)")
	piiKeyPattern := fs.String("pii-key-pattern", "", "extra case-insensitive regex OR-ed with the built-in PII denylist (forces matching attrmap keys to identity/masked)")
	dryRun := fs.Bool("dry-run", false, "discover + build map, no writes")
	fs.Parse(args)

	srcCmd, dstCmd, err := cf.resolve()
	if err != nil {
		return err
	}
	if *sourceDB == "" {
		return fmt.Errorf("--source-db is required")
	}
	key, err := loadKey(*keyFile)
	if err != nil {
		return err
	}
	cfg := discover.Config{
		Source:            srcCmd,
		Dest:              dstCmd,
		SourceDB:          *sourceDB,
		DestDB:            *destDB,
		MetaDB:            *cf.metaDB,
		WindowDays:        *window,
		SampleRows:        *sample,
		AttrCardThreshold: *attrCardThreshold,
		PIIKeyPattern:     *piiKeyPattern,
		HMACKey:           key,
		DryRun:            *dryRun,
		Model:             *model,
		Log:               logf(time.Now()),
	}
	if *serviceUsers != "" {
		cfg.ServiceUsers = strings.Split(*serviceUsers, ",")
	}
	if *keepAttrKeys != "" {
		for _, k := range strings.Split(*keepAttrKeys, ",") {
			if k = strings.TrimSpace(k); k != "" {
				cfg.KeepAttrKeys = append(cfg.KeepAttrKeys, k)
			}
		}
	}
	r, err := discover.NewRun(cfg, chclient.NewFromString(srcCmd), chclient.NewFromString(dstCmd))
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := r.Execute(ctx); err != nil {
		return err
	}
	fmt.Printf("run %s complete: %d tables, %d sandboxed, sandbox db %s, meta db %s\n",
		r.RunID, len(r.Tables), len(r.SandboxRows), r.Cfg.DestDB, *cf.metaDB)
	return nil
}

var printSections = map[string]string{
	"shape":        "SELECT key, value FROM %s.profile_shape WHERE run_id = '%s' ORDER BY key",
	"catalog":      "SELECT db_token, table_token, engine, role, total_rows, sandboxed, sandbox_rows FROM %s.profile_catalog WHERE run_id = '%s' ORDER BY total_rows DESC",
	"workload":     "SELECT db_token, table_token, execs, sels, ins, users_tok FROM %s.profile_workload WHERE run_id = '%s' ORDER BY execs DESC",
	"relations":    "SELECT rel_kind, src_db_tok, src_tbl_tok, dst_db_tok, dst_tbl_tok FROM %s.profile_relations WHERE run_id = '%s' ORDER BY rel_kind",
	"conventions":  "SELECT db_token, table_token, metric, numerator, denominator, convention FROM %s.profile_conventions WHERE run_id = '%s'",
	"verification": "SELECT claim_type, subject, status, detail FROM %s.profile_verification WHERE run_id = '%s'",
	"manifest":     "SELECT run_id, started, finished, status, stats, notes FROM %s.manifest WHERE run_id = '%s'",
}

// cmdPrint reads the profile from the DEST cluster — that is where the
// tokens-only profile lives.
func cmdPrint(args []string) error {
	fs := flag.NewFlagSet("print", flag.ExitOnError)
	cf := commonFlags(fs)
	section := fs.String("section", "shape", "profile section")
	fs.Parse(args)

	tpl, ok := printSections[*section]
	if !ok {
		return fmt.Errorf("unknown section %q", *section)
	}
	_, dstCmd, err := cf.resolve()
	if err != nil {
		return err
	}
	ex := chclient.NewFromString(dstCmd)
	st := store.New(ex, *cf.metaDB)
	ctx := context.Background()
	runID, err := st.LatestCompleteRun(ctx)
	if err != nil {
		return err
	}
	if runID == "" {
		return fmt.Errorf("no complete run in %s.manifest", *cf.metaDB)
	}
	rows, err := ex.Query(ctx, fmt.Sprintf(tpl, "`"+*cf.metaDB+"`", runID))
	if err != nil {
		return err
	}
	fmt.Println(strings.Join(rows.Names, "\t"))
	for _, row := range rows.Data {
		cells := make([]string, len(row))
		for i, c := range row {
			if c != nil {
				cells[i] = *c
			} else {
				cells[i] = `\N`
			}
		}
		fmt.Println(strings.Join(cells, "\t"))
	}
	return nil
}

// destDBFromManifest reads the dest sandbox DB name back from the manifest
// stats JSON — the run records it there precisely so verify/inspection can
// locate the sandbox without re-supplying --dest-db.
func destDBFromManifest(ctx context.Context, ex chclient.Executor, metaDB, runID string) (string, error) {
	rows, err := ex.Query(ctx, fmt.Sprintf(
		"SELECT stats FROM `%s`.manifest WHERE run_id = '%s' ORDER BY finished DESC LIMIT 1", metaDB, runID))
	if err != nil {
		return "", err
	}
	if len(rows.Data) == 0 || rows.Data[0][0] == nil {
		return "", fmt.Errorf("no manifest row for run %s", runID)
	}
	var stats struct {
		DestDB string `json:"dest_db"`
	}
	if err := json.Unmarshal([]byte(*rows.Data[0][0]), &stats); err != nil {
		return "", fmt.Errorf("manifest stats JSON: %w", err)
	}
	if stats.DestDB == "" {
		return "", fmt.Errorf("manifest stats carry no dest_db (pre-cross-cluster run?)")
	}
	return stats.DestDB, nil
}

// cmdVerify: acceptance checks against the latest complete run.
//
//	A no-survivor: no real identifier (from the SOURCE identifier_map)
//	  appears in any LLM-readable profile table or sandbox DDL on the DEST.
//	  Kept-verbatim entries (system/default, the disclosed DB name) have
//	  original == token and are excluded from the survivor list by the query.
//	B sandbox integrity: every sandboxed table on the dest queryable.
//	C trusted split: the dest meta DB holds none of the trusted tables
//	  (cross-cluster only).
func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	cf := commonFlags(fs)
	fs.Parse(args)
	srcCmd, dstCmd, err := cf.resolve()
	if err != nil {
		return err
	}
	srcEx := chclient.NewFromString(srcCmd)
	dstEx := chclient.NewFromString(dstCmd)
	dstSt := store.New(dstEx, *cf.metaDB)
	ctx := context.Background()
	// the manifest (and with it the run protocol) lives on the dest
	runID, err := dstSt.LatestCompleteRun(ctx)
	if err != nil {
		return err
	}
	if runID == "" {
		return fmt.Errorf("no complete run")
	}
	destDB, err := destDBFromManifest(ctx, dstEx, *cf.metaDB, runID)
	if err != nil {
		return err
	}

	// real identifiers of the scope (from the SOURCE trusted map — originals)
	rows, err := srcEx.Query(ctx, fmt.Sprintf(
		"SELECT DISTINCT original FROM `%s`.identifier_map WHERE run_id = '%s' AND original != token AND length(original) >= 4",
		*cf.metaDB, runID))
	if err != nil {
		return err
	}
	var reals []string
	for _, r := range rows.Data {
		if r[0] != nil {
			reals = append(reals, *r[0])
		}
	}

	// concatenate the content-bearing columns of the LLM-readable profile (DEST)
	var sb strings.Builder
	for tbl, cols := range store.ProfileContentColumns {
		res, err := dstEx.Query(ctx, fmt.Sprintf(
			"SELECT %s FROM `%s`.%s WHERE run_id = '%s'", strings.Join(cols, ", "), *cf.metaDB, tbl, runID))
		if err != nil {
			return err
		}
		for _, row := range res.Data {
			for _, c := range row {
				if c != nil {
					sb.WriteString(*c)
					sb.WriteByte('\n')
				}
			}
		}
	}
	blob := sb.String()
	survivors := 0
	for _, real := range reals {
		if discover.WordPresent(blob, real) {
			survivors++
			fmt.Printf("SURVIVOR: a real identifier (len %d) appears in profile output\n", len(real))
		}
	}

	// sandbox integrity + DDL leak re-check (DEST, tables under destDB)
	objs, err := dstSt.RegisteredObjects(ctx)
	if err != nil {
		return err
	}
	badSandbox := 0
	for _, full := range objs["table"] {
		parts := strings.SplitN(full, ".", 2)
		if len(parts) != 2 || parts[0] != destDB || !token.ReservedRe.MatchString(parts[1]) {
			continue
		}
		if _, err := dstEx.Query(ctx, fmt.Sprintf("SELECT count() FROM `%s`.`%s`", parts[0], parts[1])); err != nil {
			badSandbox++
			fmt.Printf("UNQUERYABLE: %s: %v\n", full, err)
			continue
		}
		sc, err := dstEx.Query(ctx, fmt.Sprintf("SHOW CREATE TABLE `%s`.`%s`", parts[0], parts[1]))
		if err != nil {
			return err
		}
		ddl := ""
		if len(sc.Data) > 0 && sc.Data[0][0] != nil {
			ddl = *sc.Data[0][0]
		}
		for _, real := range reals {
			if discover.WordPresent(ddl, real) {
				badSandbox++
				fmt.Printf("DDL LEAK in %s (identifier len %d)\n", full, len(real))
			}
		}
	}

	// trusted split: no trusted table may exist on the dest meta DB
	trustedOnDest := 0
	if srcCmd != dstCmd {
		quoted := make([]string, len(store.TrustedTables))
		for i, t := range store.TrustedTables {
			quoted[i] = "'" + t + "'"
		}
		res, err := dstEx.Query(ctx, fmt.Sprintf(
			"SELECT name FROM system.tables WHERE database = '%s' AND name IN (%s)",
			store.SQLEsc(*cf.metaDB), strings.Join(quoted, ",")))
		if err != nil {
			return err
		}
		for _, row := range res.Data {
			trustedOnDest++
			fmt.Printf("TRUSTED TABLE ON DEST: %s.%s\n", *cf.metaDB, *row[0])
		}
	}

	if survivors+badSandbox+trustedOnDest > 0 {
		return fmt.Errorf("verify FAILED: %d survivors, %d sandbox problems, %d trusted tables on dest",
			survivors, badSandbox, trustedOnDest)
	}
	fmt.Printf("verify OK: run %s, %d real identifiers checked, no survivors, sandbox %s clean\n", runID, len(reals), destDB)
	return nil
}

// protectedDBs may never be dropped, registry or not — a corrupted registry
// must not be able to take out a system database.
var protectedDBs = map[string]bool{
	"system": true, "default": true,
	"INFORMATION_SCHEMA": true, "information_schema": true,
	"_temporary_and_external_tables": true,
}

// cmdCleanup drops ONLY objects listed in the DEST registry (registry-listed
// ⇒ ours: ensureOurs aborts at create time if a name pre-exists
// unregistered), and optionally the meta DB on both clusters.
func cmdCleanup(args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ExitOnError)
	cf := commonFlags(fs)
	includeMeta := fs.Bool("include-meta", false, "also drop the meta database on both clusters")
	fs.Parse(args)
	srcCmd, dstCmd, err := cf.resolve()
	if err != nil {
		return err
	}
	dstEx := chclient.NewFromString(dstCmd)
	dstSt := store.New(dstEx, *cf.metaDB)
	ctx := context.Background()
	objs, err := dstSt.RegisteredObjects(ctx)
	if err != nil {
		return fmt.Errorf("read registry (does %s.generated_objects exist on the dest?): %w", *cf.metaDB, err)
	}
	for _, full := range objs["table"] {
		parts := strings.SplitN(full, ".", 2)
		if len(parts) != 2 || protectedDBs[parts[0]] {
			fmt.Printf("refusing to touch %s\n", full)
			continue
		}
		if err := dstEx.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS `%s`.`%s`", parts[0], parts[1])); err != nil {
			return err
		}
		fmt.Printf("dropped table %s\n", full)
	}
	for _, db := range objs["database"] {
		if protectedDBs[db] {
			fmt.Printf("refusing to touch database %s\n", db)
			continue
		}
		if err := dstEx.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", db)); err != nil {
			return err
		}
		fmt.Printf("dropped database %s\n", db)
	}
	if *includeMeta {
		if protectedDBs[*cf.metaDB] {
			return fmt.Errorf("refusing to drop protected database %q", *cf.metaDB)
		}
		if err := dstEx.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", *cf.metaDB)); err != nil {
			return err
		}
		fmt.Printf("dropped meta database %s (dest)\n", *cf.metaDB)
		if srcCmd != dstCmd {
			srcEx := chclient.NewFromString(srcCmd)
			if err := srcEx.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", *cf.metaDB)); err != nil {
				return err
			}
			fmt.Printf("dropped meta database %s (source)\n", *cf.metaDB)
		}
	}
	return nil
}
