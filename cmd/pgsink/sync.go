package main

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/orlandoburli/apiary-pgsink/internal/catalog"
	"github.com/orlandoburli/apiary-pgsink/internal/pipeline"
	"github.com/orlandoburli/apiary-pgsink/internal/wake"
)

func newSyncCmd() *cobra.Command {
	var configPath string
	var tables []string
	var once, verbose bool

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Follow the Apiary database and keep the target up to date",
		Long: `Runs incremental passes until interrupted.

What "changed" means depends on how Apiary writes the table:

  append_only    rows past the watermark
  mutable        rows at or after the watermark, stepped back by sync.overlap
  open_row       rows past the watermark, plus every row that has not settled
  follow_parent  rows whose parent has moved
  snapshot       the whole table, which is small

The open_row rescan is what keeps cost and token totals honest. task_executions
and step_runs are inserted at dispatch and updated at completion, and carry no
updated_at — so a cursor alone would replicate the row with status='running' and
zero cost and never see the rest.

With source.wake configured, the daemon's event stream nudges the loop and
sync.interval becomes the fallback rather than the pace.`,
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
				writeln(out, "the target is missing %d %s; run `pgsink migrate` first",
					len(changes), plural(len(changes), "change", "changes"))
				for _, c := range changes {
					if c.Blocking {
						writeln(out, "blocked  %s: %s", c.Table, c.Reason)
					}
				}
				return errBlocked
			}

			cat, err := catalog.Load()
			if err != nil {
				return err
			}
			interval, err := s.Plan.IntervalDuration()
			if err != nil {
				return err
			}

			opts := pipeline.LoopOptions{
				Instance: s.File.Source.Instance,
				Interval: interval,
				Once:     once,
				OnPass: func(p *pipeline.Pass) {
					if verbose || p.Rows > 0 {
						writeln(out, "%s  %s", time.Now().Format("15:04:05"), p)
					}
					if verbose {
						for _, r := range p.Results {
							if r.Rows > 0 {
								writeln(out, "           %-30s %6d rows", r.Table, r.Rows)
							}
						}
					}
				},
				// A transient failure costs a pass, not the process. The sink
				// runs beside a daemon that gets restarted and upgraded.
				OnError: func(err error, backoff time.Duration) bool {
					writeln(out, "%s  pass failed, retrying in %s: %v",
						time.Now().Format("15:04:05"), backoff.Round(time.Second), err)
					return true
				},
			}

			if addr := s.File.Source.Wake; addr != "" && !once {
				if ch, err := startWaker(ctx, addr); err != nil {
					writeln(out, "event stream unavailable, falling back to the %s interval: %v", interval, err)
				} else {
					opts.Wake = ch
				}
			}

			runner := &pipeline.Runner{
				Source: s.Source, Live: s.Live, Target: db, Plan: s.Plan, Catalog: cat, Out: out,
			}
			if !once {
				writeln(out, "following %d %s every %s — ctrl-c to stop",
					len(s.Plan.Tables), plural(len(s.Plan.Tables), "table", "tables"), interval)
			}
			return runner.Loop(ctx, opts)
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "pgsink.yaml", "path to pgsink.yaml")
	cmd.Flags().StringSliceVar(&tables, "tables", nil, "restrict to these tables")
	cmd.Flags().BoolVar(&once, "once", false, "run a single pass and exit")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "report every pass, including empty ones")
	return cmd
}

// startWaker connects to the daemon's event stream and reconnects if it drops.
// The stream is a latency optimisation, never a source of truth, so losing it
// degrades to the poll interval instead of stopping the sink.
func startWaker(ctx context.Context, addr string) (<-chan struct{}, error) {
	w, err := wake.New(addr)
	if err != nil {
		return nil, err
	}
	ch := make(chan struct{}, 1)
	go func() {
		backoff := time.Second
		for ctx.Err() == nil {
			if err := w.Run(ctx, ch); err != nil && ctx.Err() == nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				if backoff *= 2; backoff > time.Minute {
					backoff = time.Minute
				}
				continue
			}
			backoff = time.Second
		}
	}()
	return ch, nil
}
