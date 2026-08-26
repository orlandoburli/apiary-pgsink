package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/orlandoburli/apiary-pgsink/internal/catalog"
	"github.com/orlandoburli/apiary-pgsink/internal/config"
	sqlitesrc "github.com/orlandoburli/apiary-pgsink/internal/source/sqlite"
)

func newDoctorCmd() *cobra.Command {
	var dbPath, configPath string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the table catalog against a live Apiary database",
		Long: `Reflects the schema of a running Apiary installation and compares it
against pgsink's table catalog, and validates a configuration file against both.

Errors mean replication would be incorrect — a cursor or key column pgsink
depends on has changed. Warnings mean something moved that is worth a look but
is still safe. Columns Apiary has added are deliberately not reported: pgsink
replicates what it reflects, so new columns need no catalog change.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cat, err := catalog.Load()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "catalog     %d tables, schema_version %d, apiary %s\n",
				len(cat.Tables), cat.SchemaVersion, cat.ApiaryCompat)

			path, err := resolveDBPath(dbPath)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "database    %s\n\n", path)

			db, err := sqlitesrc.Open(cmd.Context(), path)
			if err != nil {
				return err
			}
			defer db.Close()

			live, err := sqlitesrc.Reflect(cmd.Context(), db)
			if err != nil {
				return err
			}
			findings := catalog.Drift(cat, live)
			for _, f := range findings {
				fmt.Fprintln(out, f)
			}
			errors, warnings := 0, 0
			for _, f := range findings {
				if f.Severity == catalog.Error {
					errors++
				} else {
					warnings++
				}
			}
			if len(findings) == 0 {
				fmt.Fprintf(out, "schema      no drift — the catalog matches all %d tables\n", len(live))
			} else {
				fmt.Fprintf(out, "\nschema      %d error(s), %d warning(s)\n", errors, warnings)
			}

			configErrs := checkConfig(out, configPath, cat, live)

			if catalog.HasErrors(findings) {
				return fmt.Errorf("the catalog does not match this Apiary database; replication would be incorrect")
			}
			if configErrs > 0 {
				return fmt.Errorf("the configuration does not fit this Apiary database")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "",
		"path to apiary.db (default: ./.apiary/apiary.db, then ~/.apiary/apiary.db)")
	cmd.Flags().StringVar(&configPath, "config", "",
		"path to pgsink.yaml; when omitted, only the schema is checked")
	return cmd
}

// checkConfig resolves a configuration against the catalog and the live schema,
// printing what it finds. Returns the number of errors.
//
// Doing this here rather than at startup is the point: an operator can see that
// a filter names a column their Apiary does not have, or that an extra field
// shadows a real one, without running a replication pass to find out.
func checkConfig(out io.Writer, path string, cat *catalog.Catalog, live catalog.LiveSchema) int {
	if path == "" {
		return 0
	}
	file, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(out, "\nconfig      %v\n", err)
		return 1
	}
	plan, errs := config.Resolve(file, cat)
	if len(errs) > 0 {
		fmt.Fprintf(out, "\nconfig      %d error(s)\n", len(errs))
		for _, e := range errs {
			fmt.Fprintf(out, "error %s\n", e)
		}
		return len(errs)
	}
	errs = plan.Check(live)
	for _, e := range errs {
		fmt.Fprintf(out, "error %s\n", e)
	}

	skipped := 0
	for _, t := range plan.Tables {
		if _, ok := live[t.Name]; !ok {
			skipped++
		}
	}
	fmt.Fprintf(out, "\nconfig      %s — %d table(s) enabled", path, len(plan.Tables))
	if skipped > 0 {
		fmt.Fprintf(out, ", %d absent from this database and skipped", skipped)
	}
	if n := len(plan.Unwindowed); n > 0 {
		fmt.Fprintf(out, "\n            %d table(s) have no time column and replicate in full: %s",
			n, strings.Join(plan.Unwindowed, ", "))
	}
	if len(errs) > 0 {
		fmt.Fprintf(out, ", %d error(s)", len(errs))
	}
	fmt.Fprintln(out)
	return len(errs)
}

// resolveDBPath mirrors where Apiary itself puts its database: a .apiary
// directory beside the config file, falling back to the user's home.
func resolveDBPath(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("--db %s: %w", explicit, err)
		}
		return explicit, nil
	}
	candidates := []string{filepath.Join(".apiary", "apiary.db")}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".apiary", "apiary.db"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("no apiary.db found in %v; pass --db", candidates)
}
