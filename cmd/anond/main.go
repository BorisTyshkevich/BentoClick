// anond — anonymized ClickHouse schema discovery (standalone v1 experiment).
//
// Subcommands:
//
//	run      full pipeline: discover, build map, write profile, materialize sandbox
//	print    render profile sections from the latest complete run
//	verify   acceptance checks against the latest run (no-survivor, sandbox leaks)
//	cleanup  drop ONLY objects this tool registered in generated_objects
//
// The HMAC key comes from ANON_HMAC_KEY (or --hmac-key-file): it determines
// every token; keep it as secret as the data.
package main

import (
	"context"
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
  run     --connection <name> [--databases a,b] [--window-days 7] [--sample-rows 1000000]
          [--meta-db altinity] [--service-users u1,u2] [--dry-run]
  print   --connection <name> [--meta-db altinity] [--section shape|catalog|workload|relations|conventions|verification|manifest]
  verify  --connection <name> [--meta-db altinity]
  cleanup --connection <name> [--meta-db altinity] [--include-meta]
HMAC key: ANON_HMAC_KEY env var or --hmac-key-file <path> (run only).`)
}

func commonFlags(fs *flag.FlagSet) (conn, metaDB *string) {
	conn = fs.String("connection", "", "clickhouse-client --connection name")
	metaDB = fs.String("meta-db", "altinity", "metadata database")
	return
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
	conn, metaDB := commonFlags(fs)
	dbs := fs.String("databases", "", "comma-separated database scope (default: all business DBs)")
	window := fs.Int("window-days", 7, "query_log mining window")
	sample := fs.Uint64("sample-rows", 1_000_000, "max sandbox rows per table")
	serviceUsers := fs.String("service-users", "", "comma-separated service users (demotion rule 2)")
	keyFile := fs.String("hmac-key-file", "", "file containing the HMAC key")
	dryRun := fs.Bool("dry-run", false, "discover + build map, no writes")
	fs.Parse(args)

	key, err := loadKey(*keyFile)
	if err != nil {
		return err
	}
	cfg := discover.Config{
		Connection: *conn,
		MetaDB:     *metaDB,
		WindowDays: *window,
		SampleRows: *sample,
		HMACKey:    key,
		DryRun:     *dryRun,
		Log:        logf(time.Now()),
	}
	if *dbs != "" {
		cfg.Databases = strings.Split(*dbs, ",")
	}
	if *serviceUsers != "" {
		cfg.ServiceUsers = strings.Split(*serviceUsers, ",")
	}
	r, err := discover.NewRun(cfg, chclient.New(*conn))
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := r.Execute(ctx); err != nil {
		return err
	}
	fmt.Printf("run %s complete: %d tables, %d sandboxed, meta db %s\n",
		r.RunID, len(r.Tables), len(r.SandboxRows), *metaDB)
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

func cmdPrint(args []string) error {
	fs := flag.NewFlagSet("print", flag.ExitOnError)
	conn, metaDB := commonFlags(fs)
	section := fs.String("section", "shape", "profile section")
	fs.Parse(args)

	tpl, ok := printSections[*section]
	if !ok {
		return fmt.Errorf("unknown section %q", *section)
	}
	ex := chclient.New(*conn)
	st := store.New(ex, *metaDB)
	ctx := context.Background()
	runID, err := st.LatestCompleteRun(ctx)
	if err != nil {
		return err
	}
	if runID == "" {
		return fmt.Errorf("no complete run in %s.manifest", *metaDB)
	}
	rows, err := ex.Query(ctx, fmt.Sprintf(tpl, "`"+*metaDB+"`", runID))
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

// cmdVerify: acceptance checks against the latest complete run.
//
//	A no-survivor: no real identifier (db/table/column of the scope) appears
//	  in any LLM-readable profile table or sandbox DDL.
//	B sandbox integrity: every sandboxed table queryable.
func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	conn, metaDB := commonFlags(fs)
	fs.Parse(args)
	ex := chclient.New(*conn)
	st := store.New(ex, *metaDB)
	ctx := context.Background()
	runID, err := st.LatestCompleteRun(ctx)
	if err != nil {
		return err
	}
	if runID == "" {
		return fmt.Errorf("no complete run")
	}

	// real identifiers of the scope (from the trusted map — originals)
	rows, err := ex.Query(ctx, fmt.Sprintf(
		"SELECT DISTINCT original FROM `%s`.identifier_map WHERE run_id = '%s' AND original != token AND length(original) >= 4",
		*metaDB, runID))
	if err != nil {
		return err
	}
	var reals []string
	for _, r := range rows.Data {
		if r[0] != nil {
			reals = append(reals, *r[0])
		}
	}

	// concatenate all LLM-readable profile text
	var sb strings.Builder
	for _, tbl := range []string{"profile_shape", "profile_catalog", "profile_columns",
		"profile_relations", "profile_workload", "profile_hot_columns",
		"profile_queries", "profile_conventions", "profile_verification"} {
		res, err := ex.Query(ctx, fmt.Sprintf(
			"SELECT * FROM `%s`.%s WHERE run_id = '%s'", *metaDB, tbl, runID))
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
		if strings.Contains(blob, real) {
			survivors++
			fmt.Printf("SURVIVOR: a real identifier (len %d) appears in profile output\n", len(real))
		}
	}

	// sandbox integrity + DDL leak re-check
	objs, err := st.RegisteredObjects(ctx)
	if err != nil {
		return err
	}
	badSandbox := 0
	for _, full := range objs["table"] {
		parts := strings.SplitN(full, ".", 2)
		if len(parts) != 2 || !token.ReservedRe.MatchString(parts[0]) {
			continue
		}
		if _, err := ex.Query(ctx, fmt.Sprintf("SELECT count() FROM `%s`.`%s`", parts[0], parts[1])); err != nil {
			badSandbox++
			fmt.Printf("UNQUERYABLE: %s: %v\n", full, err)
			continue
		}
		sc, err := ex.Query(ctx, fmt.Sprintf("SHOW CREATE TABLE `%s`.`%s`", parts[0], parts[1]))
		if err != nil {
			return err
		}
		ddl := ""
		if len(sc.Data) > 0 && sc.Data[0][0] != nil {
			ddl = *sc.Data[0][0]
		}
		for _, real := range reals {
			if strings.Contains(ddl, real) {
				badSandbox++
				fmt.Printf("DDL LEAK in %s (identifier len %d)\n", full, len(real))
			}
		}
	}

	if survivors+badSandbox > 0 {
		return fmt.Errorf("verify FAILED: %d survivors, %d sandbox problems", survivors, badSandbox)
	}
	fmt.Printf("verify OK: run %s, %d real identifiers checked, no survivors, sandbox clean\n", runID, len(reals))
	return nil
}

// cmdCleanup drops ONLY registered objects (and optionally the meta DB).
func cmdCleanup(args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ExitOnError)
	conn, metaDB := commonFlags(fs)
	includeMeta := fs.Bool("include-meta", false, "also drop the meta database itself")
	fs.Parse(args)
	ex := chclient.New(*conn)
	st := store.New(ex, *metaDB)
	ctx := context.Background()
	objs, err := st.RegisteredObjects(ctx)
	if err != nil {
		return fmt.Errorf("read registry (does %s.generated_objects exist?): %w", *metaDB, err)
	}
	for _, full := range objs["table"] {
		parts := strings.SplitN(full, ".", 2)
		if len(parts) != 2 || !token.ReservedRe.MatchString(parts[0]) {
			fmt.Printf("skip non-sandbox table %s\n", full)
			continue
		}
		if err := ex.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS `%s`.`%s`", parts[0], parts[1])); err != nil {
			return err
		}
		fmt.Printf("dropped table %s\n", full)
	}
	for _, db := range objs["database"] {
		if !token.ReservedRe.MatchString(db) {
			fmt.Printf("skip non-sandbox database %s\n", db)
			continue
		}
		if err := ex.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", db)); err != nil {
			return err
		}
		fmt.Printf("dropped database %s\n", db)
	}
	if *includeMeta {
		if err := ex.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", *metaDB)); err != nil {
			return err
		}
		fmt.Printf("dropped meta database %s\n", *metaDB)
	}
	return nil
}
