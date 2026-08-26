package main

import "github.com/spf13/cobra"

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pgsink",
		Short: "Replicate an Apiary database into PostgreSQL",
		Long: `pgsink backfills Apiary's history into PostgreSQL and then follows it.

  pgsink doctor     check the catalog against a live Apiary database
  pgsink migrate    create or alter the target tables       (not yet implemented)
  pgsink backfill   load history                            (not yet implemented)
  pgsink sync       follow forever                          (not yet implemented)

Backfill and sync are the same pipeline; only the starting watermark and the
stop condition differ.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	cmd.AddCommand(newDoctorCmd())
	return cmd
}
