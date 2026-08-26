package main

import (
	"github.com/spf13/cobra"
)

func newMigrateCmd() *cobra.Command {
	var configPath string
	var tables []string
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Create or alter the target tables to fit the plan",
		Long: `Reflects the Apiary database and the target, and applies the DDL needed to
make the target able to hold what the configuration selects.

Additive only. A column whose type has changed, or one the target has that the
plan no longer wants, is reported rather than altered or dropped — both are
destructive, and that is a decision for an operator, not a sync loop.

Changes apply in one transaction, so a failed migrate leaves the target exactly
as it was.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			s, err := openSession(ctx, configPath, tables)
			if err != nil {
				return err
			}
			defer s.Close()

			db, err := openTarget(ctx, s.File)
			if err != nil {
				return err
			}
			defer db.Close()

			changes, err := plannedChanges(ctx, s, db)
			if err != nil {
				return err
			}
			if len(changes) == 0 {
				writeln(out, "target is up to date — %d %s, nothing to change",
					len(s.Plan.Tables), plural(len(s.Plan.Tables), "table", "tables"))
				return nil
			}

			blocking := 0
			for _, c := range changes {
				if c.Blocking {
					blocking++
					writeln(out, "blocked  %s: %s", c.Table, c.Reason)
					continue
				}
				writeln(out, "%s;\n", c.SQL)
			}
			if blocking > 0 {
				writeln(out, "%d %s need a decision; nothing was applied",
					blocking, plural(blocking, "difference", "differences"))
				return errBlocked
			}
			if dryRun {
				writeln(out, "-- dry run: %d %s not applied",
					len(changes), plural(len(changes), "statement", "statements"))
				return nil
			}
			if err := db.Apply(ctx, changes); err != nil {
				return err
			}
			writeln(out, "applied %d %s", len(changes), plural(len(changes), "statement", "statements"))
			return nil
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "pgsink.yaml", "path to pgsink.yaml")
	cmd.Flags().StringSliceVar(&tables, "tables", nil, "restrict to these tables")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the DDL without applying it")
	return cmd
}
