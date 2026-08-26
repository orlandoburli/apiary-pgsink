package enrich

import (
	"testing"
	"time"

	"github.com/orlandoburli/apiary-pgsink/internal/config"
	"github.com/orlandoburli/apiary-pgsink/internal/pgtype"
)

var at = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func ctx() Context { return Context{Instance: "laptop", Table: "task_executions", Now: at} }

func field(name, value string) config.ExtraField {
	return config.ExtraField{Name: name, Extra: config.Extra{Value: value}}
}

func prepare(t *testing.T, fields ...config.ExtraField) []Field {
	t.Helper()
	out, err := Prepare(fields, ctx())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return out
}

func TestPlaceholdersExpand(t *testing.T) {
	fields := prepare(t,
		field("literal", "acme"),
		field("instance", "${source.instance}"),
		field("table", "${table}"),
		field("mixed", "${source.instance}/${table}"),
	)
	want := map[string]string{
		"literal":  "acme",
		"instance": "laptop",
		"table":    "task_executions",
		"mixed":    "laptop/task_executions",
	}
	for _, f := range fields {
		got, err := f.Value(ctx(), nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if got != want[f.Name] {
			t.Errorf("%s = %v, want %q", f.Name, got, want[f.Name])
		}
	}
}

func TestEnvExpands(t *testing.T) {
	t.Setenv("PGSINK_ENRICH_TEST", "staging")
	f := prepare(t, field("env", "${env:PGSINK_ENRICH_TEST}"))[0]
	got, _ := f.Value(ctx(), nil, nil)
	if got != "staging" {
		t.Errorf("got %v, want staging", got)
	}
}

// A variable unset at run time means the environment changed under a running
// process. Empty is the honest answer; failing the batch would be worse.
func TestUnsetEnvBecomesEmptyAtRunTime(t *testing.T) {
	f := prepare(t, field("env", "${env:PGSINK_DEFINITELY_UNSET}"))[0]
	got, err := f.Value(ctx(), nil, nil)
	if err != nil {
		t.Fatalf("an unset variable must not fail a batch: %v", err)
	}
	if got != "" {
		t.Errorf("got %v, want an empty string", got)
	}
}

// ${now} must be one instant for the whole run, not a value that drifts down
// the file as rows are written.
func TestNowIsResolvedOnceAndTyped(t *testing.T) {
	f := prepare(t, config.ExtraField{Name: "ingested_at", Extra: config.Extra{Value: "${now}"}})[0]
	if f.PerRow {
		t.Error("${now} does not depend on a row and must be resolved once")
	}
	if f.Type != pgtype.TimestampTZ {
		t.Errorf("type = %q, want timestamptz", f.Type)
	}
	got, _ := f.Value(ctx(), nil, nil)
	ts, ok := got.(time.Time)
	if !ok {
		t.Fatalf("got %T, want a time.Time so the driver binds it as a timestamp", got)
	}
	if !ts.Equal(at) {
		t.Errorf("got %v, want %v", ts, at)
	}
}

func TestRowReferencesArePerRow(t *testing.T) {
	f := prepare(t, field("runner_used", "${row.runner}"))[0]
	if !f.PerRow {
		t.Fatal("a template reading a row column must be evaluated per row")
	}
	columns := []string{"id", "runner"}
	for _, c := range []struct{ in, want any }{
		{"claude-cli", "claude-cli"},
		{nil, ""},
		{[]byte("codex-cli"), "codex-cli"},
		{int64(42), "42"},
	} {
		got, err := f.Value(ctx(), columns, []any{1, c.in})
		if err != nil {
			t.Fatalf("%v: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("row value %v rendered as %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMissingRowColumnIsAnError(t *testing.T) {
	f := prepare(t, field("x", "${row.nonexistent}"))[0]
	if _, err := f.Value(ctx(), []string{"id"}, []any{1}); err == nil {
		t.Fatal("reading a column that is not in the row must be an error")
	}
}

// A constant field is computed once for the whole run rather than per row: a
// pass injecting four fields into a million rows should not re-expand them a
// million times.
func TestConstantFieldsAreComputedOnce(t *testing.T) {
	fields := prepare(t, field("a", "acme"), field("b", "${table}"), field("c", "${row.runner}"))
	perRow := 0
	for _, f := range fields {
		if f.PerRow {
			perRow++
		}
	}
	if perRow != 1 {
		t.Errorf("%d of 3 fields are per-row, want only the one reading a row column", perRow)
	}
}

// A declared timestamptz that holds something unparseable falls back to the
// run's instant rather than failing or writing nonsense.
func TestDeclaredTimestampFallsBackToNow(t *testing.T) {
	f := prepare(t, config.ExtraField{Name: "when", Extra: config.Extra{
		Value: "not a timestamp", Type: pgtype.TimestampTZ,
	}})[0]
	got, _ := f.Value(ctx(), nil, nil)
	if ts, ok := got.(time.Time); !ok || !ts.Equal(at) {
		t.Errorf("got %v, want the run's instant", got)
	}
}
