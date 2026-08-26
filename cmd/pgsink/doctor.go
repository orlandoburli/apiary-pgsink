package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/orlandoburli/apiary-pgsink/internal/catalog"
	sqlitesrc "github.com/orlandoburli/apiary-pgsink/internal/source/sqlite"
)

func newDoctorCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the table catalog against a live Apiary database",
		Long: `Reflects the schema of a running Apiary installation and compares it
against pgsink's table catalog.

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
			if len(findings) == 0 {
				fmt.Fprintf(out, "no drift — the catalog matches all %d tables\n", len(live))
				return nil
			}
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
			fmt.Fprintf(out, "\n%d error(s), %d warning(s)\n", errors, warnings)
			if catalog.HasErrors(findings) {
				return fmt.Errorf("catalog does not match this Apiary database; replication would be incorrect")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "",
		"path to apiary.db (default: ./.apiary/apiary.db, then ~/.apiary/apiary.db)")
	return cmd
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
