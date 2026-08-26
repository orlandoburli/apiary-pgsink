package pipeline_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/orlandoburli/apiary-pgsink/internal/pgtype"
	sqlitesrc "github.com/orlandoburli/apiary-pgsink/internal/source/sqlite"
	"github.com/orlandoburli/apiary-pgsink/internal/target"
)

func TestMigrateCreatesThenIsANoOp(t *testing.T) {
	db, schema := newTarget(t)
	_, live := newSource(t, nil)
	plan := only(newPlan(t, ""), "task_executions", "step_runs")
	ctx := context.Background()

	for _, tbl := range plan.Tables {
		want, err := target.Desired(tbl, live[tbl.Name])
		if err != nil {
			t.Fatalf("desired: %v", err)
		}
		have, _ := db.Reflect(ctx, tbl.Name)
		changes := target.Diff(schema, want, have)
		if len(changes) != 1 {
			t.Fatalf("%s: first pass produced %d changes, want one CREATE", tbl.Name, len(changes))
		}
		if err := db.Apply(ctx, changes); err != nil {
			t.Fatalf("apply: %v", err)
		}
		have, _ = db.Reflect(ctx, tbl.Name)
		if changes := target.Diff(schema, want, have); len(changes) != 0 {
			t.Errorf("%s: second pass wants %v, should be a no-op", tbl.Name, changes)
		}
	}
}

// An Apiary release that adds a column must flow through as an additive ALTER,
// not a catalog edit and not a manual migration.
func TestNewSourceColumnBecomesAnAddColumn(t *testing.T) {
	db, schema := newTarget(t)
	src, live := newSource(t, nil)
	plan := only(newPlan(t, ""), "task_executions")
	migrateAll(t, db, plan, live)

	mustExec(t, src, `ALTER TABLE task_executions ADD COLUMN future_metric INTEGER DEFAULT 0`)
	live2, err := sqlitesrc.Reflect(context.Background(), src)
	if err != nil {
		t.Fatalf("re-reflect: %v", err)
	}

	want, err := target.Desired(plan.Tables[0], live2["task_executions"])
	if err != nil {
		t.Fatalf("desired: %v", err)
	}
	have, _ := db.Reflect(context.Background(), "task_executions")
	changes := target.Diff(schema, want, have)
	if len(changes) != 1 {
		t.Fatalf("changes = %v, want one ADD COLUMN", changes)
	}
	if err := db.Apply(context.Background(), changes); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n := scalar[int64](t, db, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema=$1 AND table_name='task_executions' AND column_name='future_metric'`, schema); n != 1 {
		t.Error("the new column was not added to the target")
	}
}

// Changing a column's type may lose data, so migrate reports it instead of
// doing it. A sync loop must not make that call.
func TestTypeConflictBlocksRatherThanAlters(t *testing.T) {
	db, schema := newTarget(t)
	_, live := newSource(t, nil)
	plan := only(newPlan(t, ""), "task_executions")
	migrateAll(t, db, plan, live)

	if _, err := db.Pool().Exec(context.Background(), fmt.Sprintf(
		"ALTER TABLE %s.task_executions ALTER COLUMN cost_usd TYPE text", schema)); err != nil {
		t.Fatalf("simulate drift: %v", err)
	}
	want, _ := target.Desired(plan.Tables[0], live["task_executions"])
	have, _ := db.Reflect(context.Background(), "task_executions")
	changes := target.Diff(schema, want, have)

	var blocking *target.Change
	for i := range changes {
		if changes[i].Blocking {
			blocking = &changes[i]
		}
	}
	if blocking == nil {
		t.Fatalf("changes = %v, want a blocking finding", changes)
	}
	// Apply must refuse the whole set rather than doing the safe part.
	if err := db.Apply(context.Background(), changes); err == nil {
		t.Error("Apply must refuse a change set containing a blocking difference")
	}
}

// A failed migrate leaves the target exactly as it was.
func TestApplyIsAtomic(t *testing.T) {
	db, schema := newTarget(t)
	ctx := context.Background()
	err := db.Apply(ctx, []target.Change{
		{Table: "a", SQL: fmt.Sprintf("CREATE TABLE %s.first_table (id text)", schema)},
		{Table: "b", SQL: "CREATE TABLE this is not valid sql"},
	})
	if err == nil {
		t.Fatal("expected the invalid statement to fail")
	}
	if n := scalar[int64](t, db, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema=$1 AND table_name='first_table'`, schema); n != 0 {
		t.Error("the first statement survived a failed batch; Apply is not atomic")
	}
}

func TestDesiredRejectsAReservedExtraField(t *testing.T) {
	_, live := newSource(t, nil)
	plan := only(newPlan(t, fmt.Sprintf(`
defaults:
  extra_fields: {%s: mine}
`, target.InstanceColumn)), "task_executions")
	if _, err := target.Desired(plan.Tables[0], live["task_executions"]); err == nil {
		t.Fatalf("%s is written by pgsink and must not be settable as an extra field", target.InstanceColumn)
	}
}

func TestStateTableSurvivesRepeatedEnsure(t *testing.T) {
	db, _ := newTarget(t)
	ctx := context.Background()
	if err := db.EnsureSchema(ctx); err != nil {
		t.Fatalf("second EnsureSchema: %v", err)
	}
	if err := db.SetWatermark(ctx, "inst", target.Watermark{
		Table: "tasks", Kind: "timestamp", Value: "2026-01-01",
	}, 10); err != nil {
		t.Fatalf("set watermark: %v", err)
	}
	if err := db.SetWatermark(ctx, "inst", target.Watermark{
		Table: "tasks", Kind: "timestamp", Value: "2026-02-01",
	}, 5); err != nil {
		t.Fatalf("update watermark: %v", err)
	}
	marks, err := db.Watermarks(ctx, "inst")
	if err != nil {
		t.Fatalf("read watermarks: %v", err)
	}
	got := marks["tasks"]
	if got.Value != "2026-02-01" {
		t.Errorf("value = %q, want the later position", got.Value)
	}
	// rows_total accumulates: it counts what has been delivered, not what the
	// last batch happened to contain.
	if got.Rows != 15 {
		t.Errorf("rows_total = %d, want 15", got.Rows)
	}
}

func TestReflectRoundTripsEveryGeneratedType(t *testing.T) {
	db, schema := newTarget(t)
	ctx := context.Background()
	types := []pgtype.PGType{
		pgtype.Text, pgtype.BigInt, pgtype.DoublePrec, pgtype.Boolean,
		pgtype.TimestampTZ, pgtype.JSONB, pgtype.Bytea, pgtype.Numeric,
	}
	cols := ""
	for i, ty := range types {
		cols += fmt.Sprintf("c%d %s, ", i, ty)
	}
	if _, err := db.Pool().Exec(ctx, fmt.Sprintf("CREATE TABLE %s.roundtrip (%s id text)", schema, cols)); err != nil {
		t.Fatalf("create: %v", err)
	}
	have, err := db.Reflect(ctx, "roundtrip")
	if err != nil {
		t.Fatalf("reflect: %v", err)
	}
	// Every type pgsink generates must reflect back as itself, or migrate would
	// report a conflict on a table it created moments earlier.
	for i, ty := range types {
		if got := have[fmt.Sprintf("c%d", i)]; got != ty {
			t.Errorf("column c%d reflected as %q, want %q", i, got, ty)
		}
	}
}
