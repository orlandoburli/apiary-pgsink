package pipeline_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/orlandoburli/apiary-pgsink/internal/pipeline"
)

func syncOnce(t *testing.T, r *pipeline.Runner, instance string) *pipeline.Pass {
	t.Helper()
	p, err := r.Sync(context.Background(), instance)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return p
}

// THE test for the whole open_row design.
//
// CreateExecution inserts a row at dispatch with status='running' and zero cost.
// UpdateExecution fills in tokens, cost and timings when the agent finishes, and
// task_executions carries no updated_at. A cursor-only follower would replicate
// the empty row and never see the rest — every cost figure in the target would
// stay zero, and it would look like the data was simply there.
func TestOpenRowRescanPicksUpCostFilledInAfterDispatch(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, func(d *sql.DB) {
		mustExec(t, d, `INSERT INTO task_executions (id, task_id, agent_id, status, cost_usd, total_tokens, created_at)
			VALUES (1, 'task-1', 'a', 'running', 0, 0, '2026-08-01 10:00:00+00:00')`)
	})
	plan := only(newPlan(t, ""), "task_executions")
	migrateAll(t, db, plan, live)
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}

	syncOnce(t, r, "inst")
	if got := scalar[float64](t, db, fmt.Sprintf("SELECT cost_usd FROM %s.task_executions WHERE id=1", schema)); got != 0 {
		t.Fatalf("cost after dispatch = %v, want 0", got)
	}

	// The agent finishes. Note: no cursor column changes — id is unchanged and
	// there is no updated_at to move.
	mustExec(t, src, `UPDATE task_executions
		SET status='success', cost_usd=1.25, total_tokens=4200, completed_at='2026-08-01 10:05:00+00:00'
		WHERE id=1`)

	syncOnce(t, r, "inst")

	var status string
	var cost float64
	var tokens int64
	err := db.Pool().QueryRow(context.Background(), fmt.Sprintf(
		"SELECT status, cost_usd, total_tokens FROM %s.task_executions WHERE id=1", schema)).
		Scan(&status, &cost, &tokens)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "success" || cost != 1.25 || tokens != 4200 {
		t.Errorf("after completion: status=%q cost=%v tokens=%d; want success/1.25/4200 — "+
			"the open_row rescan did not pick up the update", status, cost, tokens)
	}
}

// Once a row is terminal it stops being rescanned, or the rescan would grow
// without bound and re-read the whole table forever.
func TestSettledRowsLeaveTheRescan(t *testing.T) {
	db, _ := newTarget(t)
	src, live := newSource(t, func(d *sql.DB) {
		for i := 1; i <= 5; i++ {
			mustExec(t, d, `INSERT INTO task_executions (id, task_id, agent_id, status, created_at)
				VALUES (?, ?, 'a', 'success', '2026-08-01 10:00:00+00:00')`, i, fmt.Sprintf("t-%d", i))
		}
		mustExec(t, d, `INSERT INTO task_executions (id, task_id, agent_id, status, created_at)
			VALUES (6, 't-6', 'a', 'running', '2026-08-01 10:00:00+00:00')`)
	})
	plan := only(newPlan(t, ""), "task_executions")
	migrateAll(t, db, plan, live)
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}

	syncOnce(t, r, "inst") // first pass reads all six
	pass := syncOnce(t, r, "inst")

	// Only the one unsettled row should be re-read.
	if pass.Results[0].Rows != 1 {
		t.Errorf("second pass read %d rows, want just the 1 still running", pass.Results[0].Rows)
	}
}

// A NULL state is not a settled state: a row that has not reached one yet is
// the last thing that should be treated as finished.
func TestNullStateStaysOpen(t *testing.T) {
	db, _ := newTarget(t)
	src, live := newSource(t, func(d *sql.DB) {
		mustExec(t, d, `INSERT INTO task_executions (id, task_id, agent_id, status, created_at)
			VALUES (1, 't-1', 'a', NULL, '2026-08-01 10:00:00+00:00')`)
	})
	plan := only(newPlan(t, ""), "task_executions")
	migrateAll(t, db, plan, live)
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}
	syncOnce(t, r, "inst")

	if pass := syncOnce(t, r, "inst"); pass.Results[0].Rows != 1 {
		t.Errorf("a row with no state read %d times on the second pass, want 1", pass.Results[0].Rows)
	}
}

// Append-only tables must not re-read what they have already delivered.
func TestAppendOnlyReadsOnlyNewRows(t *testing.T) {
	db, _ := newTarget(t)
	src, live := newSource(t, func(d *sql.DB) {
		for i := 1; i <= 10; i++ {
			mustExec(t, d, `INSERT INTO execution_events (schema_version, type, timestamp, metadata)
				VALUES (1, 'task.discovered', '2026-08-01 10:00:00+00:00', '{}')`)
		}
	})
	plan := only(newPlan(t, ""), "execution_events")
	migrateAll(t, db, plan, live)
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}

	if pass := syncOnce(t, r, "inst"); pass.Rows != 10 {
		t.Fatalf("first pass read %d, want 10", pass.Rows)
	}
	if pass := syncOnce(t, r, "inst"); pass.Rows != 0 {
		t.Errorf("second pass re-read %d rows; an append-only cursor should be exact", pass.Rows)
	}
	mustExec(t, src, `INSERT INTO execution_events (schema_version, type, timestamp, metadata)
		VALUES (1, 'step.failed', '2026-08-01 11:00:00+00:00', '{}')`)
	if pass := syncOnce(t, r, "inst"); pass.Rows != 1 {
		t.Errorf("third pass read %d rows, want the 1 new event", pass.Rows)
	}
}

// A mutable table follows updated_at, so an in-place update must be picked up.
func TestMutableTableFollowsUpdatedAt(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, func(d *sql.DB) {
		mustExec(t, d, `INSERT INTO internal_tasks (id, title, state, created_at, updated_at)
			VALUES ('it-1', 'first', 'registered', '2026-08-01 10:00:00+00:00', '2026-08-01 10:00:00+00:00')`)
	})
	plan := only(newPlan(t, ""), "internal_tasks")
	migrateAll(t, db, plan, live)
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}
	syncOnce(t, r, "inst")

	mustExec(t, src, `UPDATE internal_tasks SET state='done', updated_at='2026-08-01 12:00:00+00:00' WHERE id='it-1'`)
	syncOnce(t, r, "inst")

	if got := scalar[string](t, db, fmt.Sprintf(
		"SELECT state FROM %s.internal_tasks WHERE id='it-1'", schema)); got != "done" {
		t.Errorf("state = %q, want done", got)
	}
}

// The bug this design exists to avoid.
//
// Apiary writes timestamps with a local UTC offset. Compared as raw text,
// '2026-08-07 02:00:00+00:00' sorts after '2026-08-06 23:00:00-04:00' — but the
// second is an hour LATER. A watermark taken that way skips every row in
// between, permanently.
func TestTimestampCursorsAreComparedInUTC(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, func(d *sql.DB) {
		// Written first, but the later instant, and it sorts earlier as text.
		mustExec(t, d, `INSERT INTO internal_tasks (id, title, state, created_at, updated_at)
			VALUES ('later', 'later', 'done', '2026-08-06 23:00:00-04:00', '2026-08-06 23:00:00-04:00')`)
		mustExec(t, d, `INSERT INTO internal_tasks (id, title, state, created_at, updated_at)
			VALUES ('earlier', 'earlier', 'done', '2026-08-07 02:00:00+00:00', '2026-08-07 02:00:00+00:00')`)
	})
	plan := only(newPlan(t, ""), "internal_tasks")
	migrateAll(t, db, plan, live)
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}
	syncOnce(t, r, "inst")

	if n := scalar[int64](t, db, fmt.Sprintf("SELECT count(*) FROM %s.internal_tasks", schema)); n != 2 {
		t.Fatalf("replicated %d of 2 rows", n)
	}
	marks, err := db.Watermarks(context.Background(), "inst")
	if err != nil {
		t.Fatalf("watermarks: %v", err)
	}
	// 23:00 -04:00 is 03:00 UTC — the true maximum. A lexicographic MAX would
	// have recorded 02:00 and skipped the later row on the next pass.
	if got := marks["internal_tasks"].Value; got != "2026-08-07 03:00:00.000" {
		t.Errorf("watermark = %q, want the true UTC maximum 2026-08-07 03:00:00.000", got)
	}
}

// A child with no cursor of its own is re-read when its parent moves.
func TestFollowParentTracksTheParentsCursor(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, func(d *sql.DB) {
		mustExec(t, d, `INSERT INTO workflow_instances (id, workflow_id, cell_id, state, created_at, updated_at)
			VALUES ('wi-1', 'w', 'c', 'running', '2026-08-01 10:00:00+00:00', '2026-08-01 10:00:00+00:00')`)
		mustExec(t, d, `INSERT INTO workflow_instance_snapshots (instance_id, workflow_json, created_at)
			VALUES ('wi-1', '{"v":1}', '2026-08-01 10:00:00+00:00')`)
	})
	plan := only(newPlan(t, ""), "workflow_instances", "workflow_instance_snapshots")
	migrateAll(t, db, plan, live)
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}
	syncOnce(t, r, "inst")

	// The snapshot is rewritten without touching created_at — which is exactly
	// why it cannot be its own cursor.
	mustExec(t, src, `UPDATE workflow_instance_snapshots SET workflow_json='{"v":2}' WHERE instance_id='wi-1'`)
	mustExec(t, src, `UPDATE workflow_instances SET updated_at='2026-08-01 12:00:00+00:00' WHERE id='wi-1'`)
	syncOnce(t, r, "inst")

	got := scalar[string](t, db, fmt.Sprintf(
		"SELECT workflow_json->>'v' FROM %s.workflow_instance_snapshots WHERE instance_id='wi-1'", schema))
	if got != "2" {
		t.Errorf("workflow_json v = %q, want 2 — the child did not follow its parent", got)
	}
}

// A snapshot table is compared wholesale, so an in-place change with no cursor
// at all still lands.
func TestSnapshotTableIsAlwaysReRead(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, func(d *sql.DB) {
		mustExec(t, d, `INSERT INTO pr_event_watermarks (source_id, watermark)
			VALUES ('github', '2026-08-01 10:00:00+00:00')`)
	})
	plan := only(newPlan(t, ""), "pr_event_watermarks")
	migrateAll(t, db, plan, live)
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}
	syncOnce(t, r, "inst")

	mustExec(t, src, `UPDATE pr_event_watermarks SET watermark='2026-08-09 10:00:00+00:00' WHERE source_id='github'`)
	syncOnce(t, r, "inst")

	got := scalar[string](t, db, fmt.Sprintf(
		"SELECT to_char(watermark,'YYYY-MM-DD') FROM %s.pr_event_watermarks WHERE source_id='github'", schema))
	if got != "2026-08-09" {
		t.Errorf("watermark = %q, want the updated 2026-08-09", got)
	}
}

// Sync must reach the same result as a backfill, from an empty target.
func TestSyncFromZeroMatchesBackfill(t *testing.T) {
	dbA, schemaA := newTarget(t)
	srcA, live := newSource(t, seedExecutions(30))
	planA := only(newPlan(t, ""), "task_executions")
	migrateAll(t, dbA, planA, live)
	backfill(t, &pipeline.Runner{Source: srcA, Live: live, Target: dbA, Plan: planA}, "inst")

	dbB, schemaB := newTarget(t)
	srcB, liveB := newSource(t, seedExecutions(30))
	planB := only(newPlan(t, ""), "task_executions")
	migrateAll(t, dbB, planB, liveB)
	syncOnce(t, &pipeline.Runner{Source: srcB, Live: liveB, Target: dbB, Plan: planB}, "inst")

	a := scalar[int64](t, dbA, fmt.Sprintf("SELECT count(*) FROM %s.task_executions", schemaA))
	b := scalar[int64](t, dbB, fmt.Sprintf("SELECT count(*) FROM %s.task_executions", schemaB))
	if a != b || a != 30 {
		t.Errorf("backfill loaded %d, sync-from-zero loaded %d, want 30 each", a, b)
	}
}

// Filtering on an open_row table's own state column is legitimate — "only
// completed executions" is a reasonable thing to want — but it changes when
// rows appear, so it should not be discovered by accident. A row is invisible
// until it settles, and then arrives complete.
func TestFilteringOnTheStateColumnDelaysRowsUntilTheySettle(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, func(d *sql.DB) {
		mustExec(t, d, `INSERT INTO task_executions (id, task_id, agent_id, status, cost_usd, created_at)
			VALUES (1, 'task-1', 'a', 'running', 0, '2026-08-01 10:00:00+00:00')`)
	})
	plan := only(newPlan(t, `
tables:
  task_executions:
    filters: [{column: status, op: in, value: [success, failed]}]
`), "task_executions")
	migrateAll(t, db, plan, live)
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}

	syncOnce(t, r, "inst")
	if n := scalar[int64](t, db, fmt.Sprintf("SELECT count(*) FROM %s.task_executions", schema)); n != 0 {
		t.Fatalf("an unsettled row reached the target despite the filter (%d rows)", n)
	}

	mustExec(t, src, `UPDATE task_executions SET status='success', cost_usd=1.5 WHERE id=1`)
	syncOnce(t, r, "inst")

	if got := scalar[float64](t, db, fmt.Sprintf(
		"SELECT cost_usd FROM %s.task_executions WHERE id=1", schema)); got != 1.5 {
		t.Errorf("cost = %v, want 1.5 — the row should arrive complete once it settles", got)
	}
}

// An interrupted pass must repeat work rather than skip it: the watermark
// advances only after every selector has drained and committed.
func TestWatermarkDoesNotAdvancePastAFailedPass(t *testing.T) {
	db, _ := newTarget(t)
	src, live := newSource(t, seedExecutions(5))
	plan := only(newPlan(t, ""), "task_executions")
	migrateAll(t, db, plan, live)
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Sync(ctx, "inst"); err == nil {
		t.Fatal("a cancelled pass should report the cancellation")
	}
	marks, err := db.Watermarks(context.Background(), "inst")
	if err != nil {
		t.Fatalf("watermarks: %v", err)
	}
	if w, ok := marks["task_executions"]; ok && w.Value != "" {
		t.Errorf("watermark advanced to %q despite the pass failing", w.Value)
	}
}
