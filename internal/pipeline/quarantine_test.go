package pipeline_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/orlandoburli/apiary-pgsink/internal/pipeline"
	"github.com/orlandoburli/apiary-pgsink/internal/target"
)

// A batch is one transaction, so one row the target refuses fails the whole
// batch — and if that row sits inside the watermark's range it fails the same
// batch on every pass, forever. The sink would stall on a single value and
// report a repeating error.
//
// The bad row is filed instead, the rest of the batch lands, and the watermark
// advances.
func TestOnePoisonRowDoesNotStallTheBatch(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, func(d *sql.DB) {
		for i := 1; i <= 5; i++ {
			mustExec(t, d, `INSERT INTO workflow_instances (id, workflow_id, cell_id, state, created_at, updated_at)
				VALUES (?, 'w', 'c', 'done', '2026-08-01 10:00:00+00:00', '2026-08-01 10:00:00+00:00')`,
				fmt.Sprintf("wi-%d", i))
		}
		// structured_output is declared jsonb in the target. This value is not
		// JSON, so PostgreSQL refuses precisely this row and no other.
		mustExec(t, d, `INSERT INTO step_runs (id, workflow_instance_id, step_id, state, structured_output)
			VALUES ('good-1', 'wi-1', 's', 'passed', '{"ok":true}'),
			       ('poison', 'wi-2', 's', 'passed', 'this is definitely not json'),
			       ('good-2', 'wi-3', 's', 'passed', '{"ok":true}')`)
	})
	plan := only(newPlan(t, ""), "step_runs")
	migrateAll(t, db, plan, live)
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}

	results, err := r.Backfill(context.Background(), "inst")
	if err != nil {
		t.Fatalf("one bad row must not fail the pass: %v", err)
	}
	if results[0].Quarantined != 1 {
		t.Errorf("quarantined %d rows, want 1", results[0].Quarantined)
	}
	if n := scalar[int64](t, db, fmt.Sprintf("SELECT count(*) FROM %s.step_runs", schema)); n != 2 {
		t.Errorf("target has %d rows, want the 2 good ones to have landed", n)
	}
}

// The quarantine has to be worth looking at: the row, the reason and enough
// identity to find it in the source.
func TestQuarantineRecordsTheRowAndTheReason(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, func(d *sql.DB) {
		mustExec(t, d, `INSERT INTO workflow_instances (id, workflow_id, cell_id, state, created_at, updated_at)
			VALUES ('wi-1', 'w', 'c', 'done', '2026-08-01 10:00:00+00:00', '2026-08-01 10:00:00+00:00')`)
		mustExec(t, d, `INSERT INTO step_runs (id, workflow_instance_id, step_id, state, structured_output)
			VALUES ('poison', 'wi-1', 'build', 'passed', 'not json')`)
	})
	plan := only(newPlan(t, ""), "step_runs")
	migrateAll(t, db, plan, live)
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}
	if _, err := r.Backfill(context.Background(), "inst"); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var table, key, reason, stepID, instance string
	err := db.Pool().QueryRow(context.Background(), fmt.Sprintf(
		`SELECT table_name, row_key, error_message, row_data->>'step_id', %s
		 FROM %s.%s`, target.InstanceColumn, schema, target.QuarantineTable)).
		Scan(&table, &key, &reason, &stepID, &instance)
	if err != nil {
		t.Fatalf("read quarantine: %v", err)
	}
	if table != "step_runs" || key != "poison" || stepID != "build" || instance != "inst" {
		t.Errorf("quarantine row = %s/%s/%s/%s, want step_runs/poison/build/inst", table, key, stepID, instance)
	}
	if reason == "" {
		t.Error("no reason recorded; the whole point is being able to see why")
	}
}

// The watermark must still advance, or the same batch is retried forever and
// the quarantine grows a duplicate on every pass.
func TestWatermarkAdvancesPastAQuarantinedRow(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, func(d *sql.DB) {
		mustExec(t, d, `INSERT INTO workflow_instances (id, workflow_id, cell_id, state, created_at, updated_at)
			VALUES ('wi-1', 'w', 'c', 'done', '2026-08-01 10:00:00+00:00', '2026-08-01 10:00:00+00:00')`)
		mustExec(t, d, `INSERT INTO step_runs (id, workflow_instance_id, step_id, state, structured_output, finished_at)
			VALUES ('poison', 'wi-1', 's', 'passed', 'not json', '2026-08-01 10:01:00+00:00')`)
	})
	plan := only(newPlan(t, ""), "step_runs")
	migrateAll(t, db, plan, live)
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}

	if _, err := r.Backfill(context.Background(), "inst"); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if _, err := r.Sync(context.Background(), "inst"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := r.Sync(context.Background(), "inst"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	n := scalar[int64](t, db, fmt.Sprintf("SELECT count(*) FROM %s.%s", schema, target.QuarantineTable))
	if n > 2 {
		t.Errorf("quarantine has %d entries after three passes; the row is being retried forever", n)
	}
}

// A good batch must not pay for the isolating path.
func TestCleanBatchesStayOnTheFastPath(t *testing.T) {
	db, _ := newTarget(t)
	src, live := newSource(t, seedExecutions(50))
	plan := only(newPlan(t, ""), "task_executions")
	migrateAll(t, db, plan, live)
	r := &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}

	results, err := r.Backfill(context.Background(), "inst")
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if results[0].Quarantined != 0 {
		t.Errorf("quarantined %d rows from a clean batch", results[0].Quarantined)
	}
	if results[0].Rows != 50 {
		t.Errorf("wrote %d rows, want 50", results[0].Rows)
	}
}
