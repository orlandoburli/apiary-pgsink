package pipeline_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/orlandoburli/apiary-pgsink/internal/pipeline"
)

// seedExecutions inserts n task_executions rows, alternating terminal status.
func seedExecutions(n int) func(*sql.DB) {
	return func(db *sql.DB) {
		for i := 1; i <= n; i++ {
			status := "success"
			if i%2 == 0 {
				status = "failed"
			}
			_, err := db.Exec(`INSERT INTO task_executions
				(task_id, agent_id, status, runner, model, cost_usd, total_tokens, can_retry, created_at, completed_at)
				VALUES (?, ?, ?, 'claude-cli', 'opus', ?, ?, ?, ?, ?)`,
				fmt.Sprintf("task-%d", i), "agent-a", status, float64(i)/10, i*100, i%2 == 0,
				"2026-08-01 10:00:00+00:00", "2026-08-01 10:05:00+00:00")
			if err != nil {
				panic(err)
			}
		}
	}
}

func backfill(t *testing.T, r *pipeline.Runner, instance string) []pipeline.TableResult {
	t.Helper()
	out, err := r.Backfill(context.Background(), instance)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	return out
}

func TestBackfillLoadsRows(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, seedExecutions(5))
	plan := only(newPlan(t, ""), "task_executions")
	migrateAll(t, db, plan, live)

	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}
	results := backfill(t, r, "inst-a")

	if len(results) != 1 || results[0].Rows != 5 {
		t.Fatalf("results = %+v, want 5 rows", results)
	}
	if n := scalar[int64](t, db, fmt.Sprintf("SELECT count(*) FROM %s.task_executions", schema)); n != 5 {
		t.Errorf("target has %d rows, want 5", n)
	}
}

// Re-running a backfill must land on the same result. Every delivery path in
// this design — a re-run, a cursor overlap, an open-row rescan, a retry after a
// crash — can deliver the same row twice.
func TestBackfillIsIdempotent(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, seedExecutions(20))
	plan := only(newPlan(t, ""), "task_executions")
	migrateAll(t, db, plan, live)

	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}
	backfill(t, r, "inst-a")
	backfill(t, r, "inst-a")
	backfill(t, r, "inst-a")

	if n := scalar[int64](t, db, fmt.Sprintf("SELECT count(*) FROM %s.task_executions", schema)); n != 20 {
		t.Errorf("target has %d rows after three passes, want 20", n)
	}
}

// The instance is part of the primary key precisely so two Apiary installations
// can share a target without the second silently overwriting the first — their
// autoincrement ids collide by construction.
func TestTwoInstancesDoNotCollide(t *testing.T) {
	db, schema := newTarget(t)
	srcA, live := newSource(t, seedExecutions(3))
	srcB, _ := newSource(t, seedExecutions(3))
	plan := only(newPlan(t, ""), "task_executions")
	migrateAll(t, db, plan, live)

	backfill(t, &pipeline.Runner{Source: srcA, Live: live, Target: db, Plan: plan}, "laptop")
	backfill(t, &pipeline.Runner{Source: srcB, Live: live, Target: db, Plan: plan}, "server")

	if n := scalar[int64](t, db, fmt.Sprintf("SELECT count(*) FROM %s.task_executions", schema)); n != 6 {
		t.Errorf("target has %d rows, want 3 from each instance", n)
	}
	if n := scalar[int64](t, db, fmt.Sprintf(
		"SELECT count(DISTINCT apiary_instance) FROM %s.task_executions", schema)); n != 2 {
		t.Errorf("distinct instances = %d, want 2", n)
	}
}

// Filters compile into the SQLite read, so rejected rows are never read at all.
func TestFiltersArePushedIntoTheRead(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, seedExecutions(10))
	plan := only(newPlan(t, `
tables:
  task_executions:
    filters: [{column: status, op: eq, value: success}]
`), "task_executions")
	migrateAll(t, db, plan, live)

	results := backfill(t, &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}, "inst-a")
	if results[0].Rows != 5 {
		t.Errorf("read %d rows, want only the 5 successes", results[0].Rows)
	}
	if n := scalar[int64](t, db, fmt.Sprintf(
		"SELECT count(*) FROM %s.task_executions WHERE status <> 'success'", schema)); n != 0 {
		t.Errorf("%d filtered rows reached the target", n)
	}
}

func TestExcludedColumnsAreNeverCreatedOrRead(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, seedExecutions(2))
	plan := only(newPlan(t, `
defaults:
  exclude_columns: [input_prompt, output_text]
`), "task_executions")
	migrateAll(t, db, plan, live)

	backfill(t, &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}, "inst-a")
	for _, col := range []string{"input_prompt", "output_text"} {
		n := scalar[int64](t, db, `SELECT count(*) FROM information_schema.columns
			WHERE table_schema=$1 AND table_name='task_executions' AND column_name=$2`, schema, col)
		if n != 0 {
			t.Errorf("%s exists in the target despite being excluded", col)
		}
	}
}

func TestExtraFieldsAreWritten(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, seedExecutions(3))
	t.Setenv("PGSINK_TEST_ENV", "staging")
	plan := only(newPlan(t, `
defaults:
  extra_fields:
    tenant_id: acme
    ingested_at: "${now}"
    deploy_env: "${env:PGSINK_TEST_ENV}"
tables:
  task_executions:
    extra_fields:
      runner_used: "${row.runner}"
      which_table: "${table}"
`), "task_executions")
	migrateAll(t, db, plan, live)
	backfill(t, &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}, "laptop")

	var tenant, env, runner, tbl, instance string
	var ingested time.Time
	err := db.Pool().QueryRow(context.Background(), fmt.Sprintf(
		`SELECT tenant_id, deploy_env, runner_used, which_table, apiary_instance, ingested_at
		 FROM %s.task_executions LIMIT 1`, schema)).
		Scan(&tenant, &env, &runner, &tbl, &instance, &ingested)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for _, c := range []struct{ got, want, name string }{
		{tenant, "acme", "tenant_id"},
		{env, "staging", "deploy_env (${env:})"},
		{runner, "claude-cli", "runner_used (${row.})"},
		{tbl, "task_executions", "which_table (${table})"},
		{instance, "laptop", "apiary_instance"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if ingested.IsZero() {
		t.Error("ingested_at (${now}) was not written as a timestamp")
	}
}

// ${now} must be one instant for the whole run, not a value that drifts down
// the file as rows are written.
func TestNowIsConstantAcrossARun(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, seedExecutions(50))
	plan := only(newPlan(t, "defaults:\n  extra_fields:\n    ingested_at: \"${now}\"\n"), "task_executions")
	plan.Sync.BatchSize = 7 // force several batches
	migrateAll(t, db, plan, live)
	backfill(t, &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}, "inst-a")

	n := scalar[int64](t, db, fmt.Sprintf(
		"SELECT count(DISTINCT ingested_at) FROM %s.task_executions", schema))
	if n != 1 {
		t.Errorf("distinct ingested_at = %d, want 1 for a single run", n)
	}
}

func TestBatchingReadsEveryRow(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, seedExecutions(101))
	plan := only(newPlan(t, ""), "task_executions")
	plan.Sync.BatchSize = 10
	migrateAll(t, db, plan, live)

	results := backfill(t, &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}, "inst-a")
	if results[0].Rows != 101 {
		t.Errorf("read %d rows across batches, want 101", results[0].Rows)
	}
	if n := scalar[int64](t, db, fmt.Sprintf("SELECT count(*) FROM %s.task_executions", schema)); n != 101 {
		t.Errorf("target has %d rows, want 101", n)
	}
}

// The watermark is taken before the scan, so rows a running daemon writes
// during a backfill sit above it and the first sync pass picks them up. Taking
// it afterwards would skip exactly those rows.
func TestWatermarkIsTakenBeforeTheScan(t *testing.T) {
	db, _ := newTarget(t)
	src, live := newSource(t, seedExecutions(5))
	plan := only(newPlan(t, ""), "task_executions")
	migrateAll(t, db, plan, live)
	backfill(t, &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}, "inst-a")

	marks, err := db.Watermarks(context.Background(), "inst-a")
	if err != nil {
		t.Fatalf("watermarks: %v", err)
	}
	mark, ok := marks["task_executions"]
	if !ok {
		t.Fatal("no watermark recorded")
	}
	if mark.Value != "5" {
		t.Errorf("watermark = %q, want the pre-scan maximum id of 5", mark.Value)
	}
	if mark.Rows != 5 {
		t.Errorf("rows_total = %d, want 5", mark.Rows)
	}
}

func TestTablesAbsentFromThisApiaryAreSkipped(t *testing.T) {
	db, _ := newTarget(t)
	src, live := newSource(t, nil)
	delete(live, "task_executions") // as an older Apiary would present it
	plan := only(newPlan(t, ""), "task_executions")

	results := backfill(t, &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}, "inst-a")
	if len(results) != 1 || !results[0].Skipped {
		t.Fatalf("results = %+v, want the table skipped rather than an error", results)
	}
}
