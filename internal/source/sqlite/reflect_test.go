package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/orlandoburli/apiary-pgsink/internal/catalog"
	"github.com/orlandoburli/apiary-pgsink/internal/fixture"
)

// The whole point of phase 0: the catalog must describe the schema Apiary
// actually ships, for every snapshot in testdata. A new snapshot that drifts
// fails here, with a message naming the table and the column.
func TestCatalogMatchesEverySnapshot(t *testing.T) {
	labels, err := fixture.Labels()
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if len(labels) == 0 {
		t.Fatal("no schema snapshots in testdata/schema")
	}
	cat, err := catalog.Load()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	for _, label := range labels {
		t.Run(label, func(t *testing.T) {
			db := fixture.Build(t, label)
			live, err := Reflect(context.Background(), db)
			if err != nil {
				t.Fatalf("reflect: %v", err)
			}
			for _, f := range catalog.Drift(cat, live) {
				if f.Severity == catalog.Error {
					t.Errorf("drift: %s", f)
				} else {
					t.Logf("drift: %s", f)
				}
			}
		})
	}
}

func TestReflectReadsKeysAndColumns(t *testing.T) {
	db := fixture.Build(t, fixture.Latest)
	live, err := Reflect(context.Background(), db)
	if err != nil {
		t.Fatalf("reflect: %v", err)
	}

	te, ok := live["task_executions"]
	if !ok {
		t.Fatal("task_executions missing")
	}
	if len(te.PrimaryKey) != 1 || te.PrimaryKey[0] != "id" {
		t.Errorf("task_executions primary key = %v, want [id]", te.PrimaryKey)
	}
	// The columns that only exist because UpdateExecution runs after the row is
	// already there. If reflection cannot see them, open_row buys us nothing.
	for _, col := range []string{"cost_usd", "total_tokens", "completed_at", "status"} {
		if !te.Has(col) {
			t.Errorf("task_executions is missing %q", col)
		}
	}

	// Composite keys must come back in declaration order, not scan order.
	ped, ok := live["pr_event_dispatches"]
	if !ok {
		t.Fatal("pr_event_dispatches missing")
	}
	want := []string{"source_id", "event_id", "workflow_id"}
	if len(ped.PrimaryKey) != len(want) {
		t.Fatalf("composite key = %v, want %v", ped.PrimaryKey, want)
	}
	for i := range want {
		if ped.PrimaryKey[i] != want[i] {
			t.Fatalf("composite key = %v, want %v", ped.PrimaryKey, want)
		}
	}
}

// New columns are not drift. An Apiary release that adds one must flow through
// without a catalog edit, or every upgrade becomes a release of this repo.
func TestAddedColumnIsNotDrift(t *testing.T) {
	db := fixture.Build(t, fixture.Latest)
	if _, err := db.Exec(`ALTER TABLE task_executions ADD COLUMN some_future_metric INTEGER DEFAULT 0`); err != nil {
		t.Fatalf("alter: %v", err)
	}
	cat, err := catalog.Load()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	live, err := Reflect(context.Background(), db)
	if err != nil {
		t.Fatalf("reflect: %v", err)
	}
	for _, f := range catalog.Drift(cat, live) {
		if f.Table == "task_executions" {
			t.Errorf("adding a column should not be drift, got: %s", f)
		}
	}
}

// Losing a cursor column must be an error, not a warning: the table silently
// stops advancing otherwise.
func TestRemovedCursorColumnIsAnError(t *testing.T) {
	db := fixture.Build(t, fixture.Latest)
	if _, err := db.Exec(`ALTER TABLE workflow_instances DROP COLUMN updated_at`); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	cat, err := catalog.Load()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	live, err := Reflect(context.Background(), db)
	if err != nil {
		t.Fatalf("reflect: %v", err)
	}
	findings := catalog.Drift(cat, live)
	if !catalog.HasErrors(findings) {
		t.Fatalf("expected an error finding, got %v", findings)
	}
}

func TestOpenReadsAnExistingDatabase(t *testing.T) {
	path := fixture.Path(t, fixture.Latest)
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	live, err := Reflect(context.Background(), db)
	if err != nil {
		t.Fatalf("reflect: %v", err)
	}
	if len(live) == 0 {
		t.Fatal("reflected no tables")
	}
}

// modernc.org/sqlite ignores unrecognised query parameters on a bare path, so
// "mode=ro" only takes effect through the file: URI form. Without it a mistyped
// --db creates an empty database instead of failing, and pgsink then replicates
// a schema that does not exist. Pin both halves.
func TestOpenIsReadOnlyAndNeverCreates(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there.db")
	if _, err := Open(context.Background(), missing); err == nil {
		t.Fatal("opening a nonexistent database must fail")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("opening a nonexistent database must not create it")
	}

	db, err := Open(context.Background(), fixture.Path(t, fixture.Latest))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE nope (x INTEGER)`); err == nil {
		t.Fatal("a read-only handle must refuse writes")
	}
}

func TestOpenAcceptsPathsWithSpaces(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := fixture.Path(t, fixture.Latest)
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dst := filepath.Join(dir, "apiary.db")
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	db, err := Open(context.Background(), dst)
	if err != nil {
		t.Fatalf("open %q: %v", dst, err)
	}
	defer db.Close()
	if _, err := Reflect(context.Background(), db); err != nil {
		t.Fatalf("reflect: %v", err)
	}
}
