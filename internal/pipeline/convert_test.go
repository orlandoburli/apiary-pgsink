package pipeline_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/orlandoburli/apiary-pgsink/internal/pipeline"
)

// Apiary has written timestamps in several shapes over its life, and a backfill
// meets all of them in one pass through the historical data.
//
// The last case is the important one. Before Apiary's _time_format fix, rows
// were stored as Go's time.Time.String() — which appends " m=+0.000000001", a
// monotonic clock reading. SQLite's own DATE()/datetime() cannot parse that, so
// Apiary silently dropped those rows from every windowed query. The sink can do
// better than silently dropping them.
func TestLegacyTimestampFormatsAreParsed(t *testing.T) {
	db, schema := newTarget(t)
	stamps := []struct {
		name  string
		value string
	}{
		{"rfc3339", "2026-08-01T10:00:00Z"},
		{"sqlite space form", "2026-08-01 10:00:00.123456789 -04:00"},
		{"no separator", "2026-08-01 10:00:00"},
		{"go String() with monotonic", "2026-08-01 10:00:00.123456789 -0400 EDT m=+0.000000001"},
		{"go String() no monotonic", "2026-08-01 10:00:00.123456789 -0400 EDT"},
	}
	src, live := newSource(t, func(d *sql.DB) {
		for i, s := range stamps {
			mustExec(t, d, `INSERT INTO task_executions (task_id, agent_id, status, created_at)
				VALUES (?, 'a', 'success', ?)`, fmt.Sprintf("t-%d", i), s.value)
		}
	})
	plan := only(newPlan(t, ""), "task_executions")
	migrateAll(t, db, plan, live)
	backfill(t, &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}, "inst-a")

	for i, s := range stamps {
		var got *time.Time
		err := db.Pool().QueryRow(context.Background(), fmt.Sprintf(
			"SELECT created_at FROM %s.task_executions WHERE task_id = $1", schema),
			fmt.Sprintf("t-%d", i)).Scan(&got)
		if err != nil {
			t.Fatalf("%s: query: %v", s.name, err)
		}
		if got == nil {
			t.Errorf("%s: %q parsed to NULL", s.name, s.value)
			continue
		}
		if got.Year() != 2026 || got.Month() != time.August || got.Day() != 1 {
			t.Errorf("%s: %q parsed to %v", s.name, s.value, got)
		}
	}
}

// An unparseable timestamp becomes NULL rather than failing the batch. One bad
// legacy row must not stop a backfill of a million good ones.
func TestUnparseableTimestampBecomesNull(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, func(d *sql.DB) {
		mustExec(t, d, `INSERT INTO task_executions (task_id, agent_id, status, created_at)
			VALUES ('bad', 'a', 'success', 'not a timestamp at all')`)
	})
	plan := only(newPlan(t, ""), "task_executions")
	migrateAll(t, db, plan, live)
	backfill(t, &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}, "inst-a")

	if n := scalar[int64](t, db, fmt.Sprintf(
		"SELECT count(*) FROM %s.task_executions WHERE created_at IS NULL", schema)); n != 1 {
		t.Errorf("unparseable timestamp should land as NULL, got %d null rows", n)
	}
}

// SQLite has no boolean storage class: a column declared BOOLEAN holds 0 and 1,
// and the driver returns int64, which PostgreSQL will not accept.
func TestBooleansAreConverted(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, func(d *sql.DB) {
		mustExec(t, d, `INSERT INTO task_executions (task_id, agent_id, status, can_retry)
			VALUES ('yes', 'a', 'success', 1), ('no', 'a', 'success', 0), ('unknown', 'a', 'success', NULL)`)
	})
	plan := only(newPlan(t, ""), "task_executions")
	migrateAll(t, db, plan, live)
	backfill(t, &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}, "inst-a")

	for _, c := range []struct {
		id   string
		want *bool
	}{{"yes", ptr(true)}, {"no", ptr(false)}, {"unknown", nil}} {
		var got *bool
		err := db.Pool().QueryRow(context.Background(), fmt.Sprintf(
			"SELECT can_retry FROM %s.task_executions WHERE task_id = $1", schema), c.id).Scan(&got)
		if err != nil {
			t.Fatalf("%s: %v", c.id, err)
		}
		switch {
		case c.want == nil && got != nil:
			t.Errorf("%s: got %v, want NULL", c.id, *got)
		case c.want != nil && (got == nil || *got != *c.want):
			t.Errorf("%s: got %v, want %v", c.id, got, *c.want)
		}
	}
}

// Empty text is not valid JSON. Writing it into a jsonb column fails the whole
// batch, so it becomes NULL — the honest equivalent of "no document".
func TestEmptyJSONBecomesNull(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, func(d *sql.DB) {
		mustExec(t, d, `INSERT INTO workflow_instances (id, workflow_id, cell_id, state, created_at, updated_at)
			VALUES ('wi-1', 'w', 'cell-1', 'done', '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`)
		mustExec(t, d, `INSERT INTO step_runs (id, workflow_instance_id, step_id, state, structured_output)
			VALUES ('sr-empty', 'wi-1', 's', 'passed', ''),
			       ('sr-json',  'wi-1', 's2', 'passed', '{"ok":true}')`)
	})
	plan := only(newPlan(t, ""), "step_runs")
	migrateAll(t, db, plan, live)
	backfill(t, &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}, "inst-a")

	if n := scalar[int64](t, db, fmt.Sprintf(
		"SELECT count(*) FROM %s.step_runs WHERE structured_output IS NULL", schema)); n != 1 {
		t.Errorf("empty JSON text should land as NULL, got %d null rows", n)
	}
	if v := scalar[bool](t, db, fmt.Sprintf(
		"SELECT (structured_output->>'ok')::boolean FROM %s.step_runs WHERE id = 'sr-json'", schema)); !v {
		t.Error("real JSON should be queryable as jsonb")
	}
}

// The since/until window keeps rows whose timestamp is unknown: a step_run that
// has not started has a NULL started_at and is the most current row there is.
func TestWindowKeepsRowsWithNoTimestamp(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, func(d *sql.DB) {
		mustExec(t, d, `INSERT INTO workflow_instances (id, workflow_id, cell_id, state, created_at, updated_at)
			VALUES ('wi-1', 'w', 'cell-1', 'running', '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`)
		mustExec(t, d, `INSERT INTO step_runs (id, workflow_instance_id, step_id, state, started_at)
			VALUES ('pending',  'wi-1', 's1', 'pending', NULL),
			       ('recent',   'wi-1', 's2', 'passed',  '2026-08-01T10:00:00Z'),
			       ('ancient',  'wi-1', 's3', 'passed',  '2020-01-01T10:00:00Z')`)
	})
	plan := only(newPlan(t, "defaults:\n  since: \"2026-01-01\"\n"), "step_runs")
	migrateAll(t, db, plan, live)
	backfill(t, &pipeline.Runner{Source: src, Live: live, Target: db, Plan: plan}, "inst-a")

	got := map[string]bool{}
	rows, err := db.Pool().Query(context.Background(), fmt.Sprintf("SELECT id FROM %s.step_runs", schema))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got[id] = true
	}
	if !got["pending"] {
		t.Error("a row with no timestamp was dropped by the window; it is work in flight")
	}
	if !got["recent"] {
		t.Error("a row inside the window was dropped")
	}
	if got["ancient"] {
		t.Error("a row outside the window was replicated")
	}
}

func ptr[T any](v T) *T { return &v }
