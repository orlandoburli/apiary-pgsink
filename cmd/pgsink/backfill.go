package main

import (
	"errors"
	"time"

	"github.com/spf13/cobra"

	"github.com/orlandoburli/apiary-pgsink/internal/pipeline"
)

var errBlocked = errors.New("target schema needs a decision; see the messages above")

func newBackfillCmd() *cobra.Command {
	var configPath string
	var tables []string
	var migrate bool

	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Load Apiary's history into the target",
		Long: `Reads every selected row and upserts it into the target.

Backfill is the same pipeline sync uses, with the watermark starting at zero and
a stop condition at the end of each table. Re-running it is safe: every write is
an idempotent upsert on the primary key.

Each table's cursor position is read before its scan and recorded afterwards, so
rows written by a running daemon during the backfill sit above the watermark and
are picked up by the first sync pass rather than being skipped.`,
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
			if len(changes) > 0 {
				if !migrate {
					writeln(out, "the target is missing %d %s; run `pgsink migrate` first, or pass --migrate",
						len(changes), plural(len(changes), "change", "changes"))
					for _, c := range changes {
						if c.Blocking {
							writeln(out, "blocked  %s: %s", c.Table, c.Reason)
						}
					}
					return errBlocked
				}
				if err := db.Apply(ctx, changes); err != nil {
					return err
				}
				writeln(out, "migrated %d %s", len(changes), plural(len(changes), "statement", "statements"))
			}

			runner := &pipeline.Runner{
				Source: s.Source, Live: s.Live, Target: db, Plan: s.Plan, Out: out,
			}
			started := time.Now()
			results, err := runner.Backfill(ctx, s.File.Source.Instance)
			for _, r := range results {
				switch {
				case r.Skipped:
					writeln(out, "%-30s skipped  %s", r.Table, r.Reason)
				default:
					writeln(out, "%-30s %8d rows  %s", r.Table, r.Rows, r.Elapsed.Round(time.Millisecond))
				}
			}
			if err != nil {
				return err
			}
			var total int64
			for _, r := range results {
				total += r.Rows
			}
			writeln(out, "\n%d rows in %s", total, time.Since(started).Round(time.Millisecond))
			return nil
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "pgsink.yaml", "path to pgsink.yaml")
	cmd.Flags().StringSliceVar(&tables, "tables", nil, "restrict to these tables")
	cmd.Flags().BoolVar(&migrate, "migrate", false, "apply any missing DDL before loading")
	return cmd
}
