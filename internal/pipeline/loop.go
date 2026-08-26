package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// LoopOptions configures the follow loop.
type LoopOptions struct {
	Instance string
	Interval time.Duration
	// Wake receives a signal when the daemon reports activity. Optional: with
	// no waker the loop simply runs on its interval.
	Wake <-chan struct{}
	// Once stops after a single pass, for `sync --once`.
	Once bool
	// OnPass is called after every pass, for reporting.
	OnPass func(*Pass)
	// OnError is called when a pass fails. Returning false stops the loop;
	// returning true retries after the backoff.
	OnError func(error, time.Duration) bool
	// MinBackoff is the first retry delay, doubling to MaxBackoff. Zero uses
	// the defaults.
	MinBackoff time.Duration
	MaxBackoff time.Duration
}

// Loop follows the source until ctx ends.
//
// A failed pass does not stop the loop. The sink runs beside a daemon that gets
// restarted, upgraded and interrupted, and a transient error — the target
// briefly unreachable, the database locked mid-checkpoint — should cost a pass,
// not the process. Backoff is exponential and capped, and every pass starts by
// re-reading the watermarks, so recovery needs no state carried across the
// failure.
func (r *Runner) Loop(ctx context.Context, opts LoopOptions) error {
	minBackoff, maxBackoff := opts.MinBackoff, opts.MaxBackoff
	if minBackoff <= 0 {
		minBackoff = time.Second
	}
	if maxBackoff <= 0 {
		maxBackoff = 2 * time.Minute
	}
	backoff := minBackoff

	timer := time.NewTimer(0)
	defer timer.Stop()
	if !timer.Stop() {
		<-timer.C
	}
	timer.Reset(0) // first pass immediately

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		case <-opts.Wake:
			// Drain anything that arrived while a pass was running, so a burst
			// of events costs one pass rather than one pass each.
			drain(opts.Wake)
		}

		pass, err := r.Sync(ctx, opts.Instance)
		switch {
		case err == nil:
			backoff = minBackoff
			if opts.OnPass != nil {
				opts.OnPass(pass)
			}
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return nil
		default:
			if opts.OnError != nil && !opts.OnError(err, backoff) {
				return err
			}
			timer.Reset(backoff)
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		if opts.Once {
			return nil
		}
		timer.Reset(opts.Interval)
	}
}

func drain(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// String renders a pass for a log line.
func (p *Pass) String() string {
	changed := 0
	for _, r := range p.Results {
		if r.Rows > 0 {
			changed++
		}
	}
	quarantined := ""
	if p.Quarantined > 0 {
		quarantined = fmt.Sprintf(", %d quarantined", p.Quarantined)
	}
	if p.Rows == 0 {
		return fmt.Sprintf("no changes%s (%s)", quarantined, p.Elapsed.Round(time.Millisecond))
	}
	return fmt.Sprintf("%d rows across %d table(s)%s (%s)",
		p.Rows, changed, quarantined, p.Elapsed.Round(time.Millisecond))
}
