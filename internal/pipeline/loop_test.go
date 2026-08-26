package pipeline_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/orlandoburli/apiary-pgsink/internal/pipeline"
)

func TestLoopOnceRunsExactlyOnePass(t *testing.T) {
	db, _ := newTarget(t)
	src, live := newSource(t, seedExecutions(3))
	plan := only(newPlan(t, ""), "task_executions")
	migrateAll(t, db, plan, live)
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}

	passes := 0
	err := r.Loop(context.Background(), pipeline.LoopOptions{
		Instance: "inst", Interval: time.Hour, Once: true,
		OnPass: func(*pipeline.Pass) { passes++ },
	})
	if err != nil {
		t.Fatalf("loop: %v", err)
	}
	if passes != 1 {
		t.Errorf("ran %d passes, want exactly 1", passes)
	}
}

// A wake signal must run a pass without waiting out the interval — that is the
// whole reason for listening to the daemon's event stream.
func TestWakeTriggersAPassBeforeTheInterval(t *testing.T) {
	db, _ := newTarget(t)
	src, live := newSource(t, seedExecutions(1))
	plan := only(newPlan(t, ""), "task_executions")
	migrateAll(t, db, plan, live)
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}

	wake := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	passes := make(chan struct{}, 8)
	go func() {
		_ = r.Loop(ctx, pipeline.LoopOptions{
			Instance: "inst",
			Interval: time.Hour, // long enough that only a wake can drive this
			Wake:     wake,
			OnPass:   func(*pipeline.Pass) { passes <- struct{}{} },
		})
	}()

	<-passes // the immediate first pass
	wake <- struct{}{}
	select {
	case <-passes:
	case <-ctx.Done():
		t.Fatal("a wake signal did not trigger a pass within the timeout")
	}
}

// The sink runs beside a daemon that gets restarted and upgraded. A transient
// failure should cost a pass, not the process.
func TestLoopRetriesAfterAFailedPass(t *testing.T) {
	db, _ := newTarget(t)
	src, live := newSource(t, seedExecutions(1))
	plan := only(newPlan(t, ""), "task_executions")
	// No migrate: the target table is missing, so every pass fails.
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	failures := 0
	err := r.Loop(ctx, pipeline.LoopOptions{
		Instance: "inst", Interval: 10 * time.Millisecond,
		MinBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
		OnError: func(error, time.Duration) bool {
			failures++
			return failures < 3 // stop after three, to keep the test quick
		},
	})
	if failures < 3 {
		t.Errorf("loop gave up after %d failures, want it to keep retrying", failures)
	}
	if err == nil && failures >= 3 {
		t.Log("loop stopped on request after repeated failures, as asked")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Error("loop should have stopped on the OnError verdict, not the timeout")
	}
}

func TestLoopStopsOnContextCancellation(t *testing.T) {
	db, _ := newTarget(t)
	src, live := newSource(t, seedExecutions(1))
	plan := only(newPlan(t, ""), "task_executions")
	migrateAll(t, db, plan, live)
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- r.Loop(ctx, pipeline.LoopOptions{Instance: "inst", Interval: 20 * time.Millisecond})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("loop returned %v, want a clean stop", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not stop when its context ended")
	}
}
