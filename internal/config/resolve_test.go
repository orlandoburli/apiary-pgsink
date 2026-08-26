package config

import (
	"strings"
	"testing"

	"github.com/orlandoburli/apiary-pgsink/internal/catalog"
	"github.com/orlandoburli/apiary-pgsink/internal/pgtype"
)

const minimal = `
source: {dsn: sqlite:///tmp/apiary.db, instance: laptop}
target: {dsn: "postgres://localhost/apiary"}
`

func parse(t *testing.T, extra string) *File {
	t.Helper()
	f, err := Parse([]byte(minimal + extra))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func resolve(t *testing.T, extra string) *Plan {
	t.Helper()
	cat, err := catalog.Load()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	plan, errs := Resolve(parse(t, extra), cat)
	if len(errs) > 0 {
		t.Fatalf("Resolve: %v", errs)
	}
	return plan
}

func table(t *testing.T, p *Plan, name string) Table {
	t.Helper()
	for _, tbl := range p.Tables {
		if tbl.Name == name {
			return tbl
		}
	}
	t.Fatalf("%s not in plan", name)
	return Table{}
}

// --- the four merge rules ---------------------------------------------------

func TestExtraFieldsMergeAndTableWins(t *testing.T) {
	p := resolve(t, `
defaults:
  extra_fields: {tenant_id: acme, env: prod}
tables:
  task_executions:
    extra_fields: {tenant_id: acme-eng, runner: "${row.runner}"}
`)
	got := map[string]string{}
	for _, e := range table(t, p, "task_executions").ExtraFields {
		got[e.Name] = e.Value
	}
	if got["tenant_id"] != "acme-eng" {
		t.Errorf("tenant_id = %q, want the table's value", got["tenant_id"])
	}
	if got["env"] != "prod" {
		t.Errorf("env = %q, want the global value to survive", got["env"])
	}
	if got["runner"] != "${row.runner}" {
		t.Errorf("runner = %q, want the table-only field", got["runner"])
	}
	// A different table keeps only the globals.
	if v := extras(table(t, p, "tasks"))["tenant_id"]; v != "acme" {
		t.Errorf("tasks tenant_id = %q, want the global value", v)
	}
}

// A global exclusion is a guarantee — usually PII or volume. A table block must
// not be able to quietly re-admit the column.
func TestExcludeColumnsUnionAndAreNeverSubtractive(t *testing.T) {
	p := resolve(t, `
defaults:
  exclude_columns: [input_prompt, output_text]
tables:
  task_executions:
    exclude_columns: [error_message]
`)
	ex := table(t, p, "task_executions").Exclude
	for _, want := range []string{"input_prompt", "output_text", "error_message"} {
		if !ex[want] {
			t.Errorf("%q should be excluded", want)
		}
	}
}

func TestIncludeColumnsAreTakenVerbatimPerTable(t *testing.T) {
	p := resolve(t, `
tables:
  step_runs:
    include_columns: [id, state, finished_at, cost_usd]
`)
	got := table(t, p, "step_runs").Include
	if len(got) != 4 || got[0] != "id" || got[3] != "cost_usd" {
		t.Errorf("include = %v, want the table's list verbatim", got)
	}
	// A table that says nothing projects everything.
	if inc := table(t, p, "tasks").Include; len(inc) != 0 {
		t.Errorf("tasks include = %v, want no projection", inc)
	}
}

// No two Apiary tables share a column set, so a global projection would omit
// some table's key or cursor and fail for most of the database. Reject it with
// an explanation rather than letting resolution produce a wall of errors.
func TestGlobalIncludeColumnsIsRejectedWithAnExplanation(t *testing.T) {
	_, err := Parse([]byte(minimal + "defaults:\n  include_columns: [id, created_at]\n"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "per table") {
		t.Errorf("error should say what to do instead: %v", err)
	}
}

func TestFiltersAreAndedNotOverridden(t *testing.T) {
	p := resolve(t, `
defaults:
  filters:
    - {column: created_at, op: gte, value: "2026-01-01"}
tables:
  task_executions:
    filters:
      - {column: status, op: in, value: [success, failed]}
`)
	got := table(t, p, "task_executions").Filters
	if len(got) != 2 {
		t.Fatalf("filters = %v, want both the global and the table filter", got)
	}
	if got[0].Column != "created_at" || got[1].Column != "status" {
		t.Errorf("filters = %v, want global first then table", got)
	}
}

func TestIgnoreGlobalFiltersIsTheOnlyEscape(t *testing.T) {
	p := resolve(t, `
defaults:
  filters:
    - {column: created_at, op: gte, value: "2026-01-01"}
tables:
  execution_events:
    ignore_global_filters: true
`)
	if got := table(t, p, "execution_events").Filters; len(got) != 0 {
		t.Errorf("filters = %v, want none", got)
	}
	if got := table(t, p, "tasks").Filters; len(got) != 1 {
		t.Errorf("other tables must keep the global filter, got %v", got)
	}
}

// Setting the escape hatch with nothing to escape means someone expected a
// global filter that is not there.
func TestIgnoreGlobalFiltersWithoutGlobalsIsRejected(t *testing.T) {
	_, err := Parse([]byte(minimal + `
tables:
  tasks: {ignore_global_filters: true}
`))
	if err == nil {
		t.Fatal("expected an error")
	}
}

// --- enablement -------------------------------------------------------------

func TestEnablement(t *testing.T) {
	p := resolve(t, `
tables:
  task_logs: {enabled: false}
  service_logs: {enabled: false}
`)
	for _, tbl := range p.Tables {
		if tbl.Name == "task_logs" || tbl.Name == "service_logs" {
			t.Errorf("%s should be disabled", tbl.Name)
		}
	}
	if len(p.Tables) != 22 {
		t.Errorf("enabled tables = %d, want 22 of 24", len(p.Tables))
	}
}

func TestDefaultsEnabledFalseInvertsTheDefault(t *testing.T) {
	p := resolve(t, `
defaults: {enabled: false}
tables:
  execution_events: {enabled: true}
  step_runs: {enabled: true}
`)
	if len(p.Tables) != 2 {
		t.Fatalf("enabled tables = %d, want just the two opted in", len(p.Tables))
	}
}

func TestEverythingDisabledIsAnError(t *testing.T) {
	cat, _ := catalog.Load()
	_, errs := Resolve(parse(t, "defaults: {enabled: false}\n"), cat)
	if len(errs) == 0 {
		t.Fatal("replicating nothing should be an error, not a quiet no-op")
	}
}

// --- guards -----------------------------------------------------------------

func TestKeyAndCursorColumnsCannotBeDroppedd(t *testing.T) {
	cat, _ := catalog.Load()
	for _, tc := range []struct{ name, yaml string }{
		{"excluded key", "tables:\n  tasks:\n    exclude_columns: [id]\n"},
		{"excluded cursor", "tables:\n  tasks:\n    exclude_columns: [updated_at]\n"},
		{"projection drops key", "tables:\n  tasks:\n    include_columns: [title, updated_at]\n"},
		{"projection drops cursor", "tables:\n  tasks:\n    include_columns: [id, title]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := Resolve(parse(t, tc.yaml), cat)
			if len(errs) == 0 {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestUnknownTableIsRejected(t *testing.T) {
	cat, _ := catalog.Load()
	_, errs := Resolve(parse(t, "tables:\n  task_executionz: {enabled: false}\n"), cat)
	if len(errs) == 0 {
		t.Fatal("a misspelt table name must not be a silent no-op")
	}
	if !strings.Contains(errs[0].Error(), "task_executionz") {
		t.Errorf("error should name the table: %v", errs[0])
	}
}

// --- Check, against a real schema ------------------------------------------

func TestCheckAgainstLiveSchema(t *testing.T) {
	live := catalog.LiveSchema{"tasks": {
		Name:       "tasks",
		PrimaryKey: []string{"id"},
		Columns: []catalog.LiveColumn{
			{Name: "id", Type: "TEXT"}, {Name: "state", Type: "TEXT"},
			{Name: "updated_at", Type: "TIMESTAMP"}, {Name: "output", Type: "TEXT"},
		},
	}}
	cat, _ := catalog.Load()

	cases := map[string]string{
		"filter on a missing column": `
tables:
  tasks:
    filters: [{column: nonexistent, op: eq, value: x}]`,
		"extra field shadows a real column": `
tables:
  tasks:
    extra_fields: {state: injected}`,
		"extra field reads a missing column": `
tables:
  tasks:
    extra_fields: {label: "${row.nonexistent}"}`,
		"extra field reads an excluded column": `
tables:
  tasks:
    exclude_columns: [output]
    extra_fields: {label: "${row.output}"}`,
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			plan, errs := Resolve(parse(t, extra), cat)
			if len(errs) > 0 {
				t.Fatalf("Resolve: %v", errs)
			}
			if got := plan.Check(live); len(got) == 0 {
				t.Fatal("expected Check to object")
			}
		})
	}
}

func TestColumnsAppliesProjectionThenExclusion(t *testing.T) {
	live := catalog.LiveTable{Name: "tasks", Columns: []catalog.LiveColumn{
		{Name: "id"}, {Name: "title"}, {Name: "output"}, {Name: "updated_at"},
	}}
	p := resolve(t, `
tables:
  tasks:
    include_columns: [id, title, output, updated_at]
    exclude_columns: [output]
`)
	got := table(t, p, "tasks").Columns(live)
	if len(got) != 3 || contains(got, "output") {
		t.Errorf("columns = %v, want the projection minus the exclusion", got)
	}
}

func TestColumnsDefaultsToEverythingLive(t *testing.T) {
	live := catalog.LiveTable{Name: "tasks", Columns: []catalog.LiveColumn{
		{Name: "id"}, {Name: "title"}, {Name: "brand_new_column"},
	}}
	p := resolve(t, "")
	got := table(t, p, "tasks").Columns(live)
	// A column Apiary added since this build must flow through untouched.
	if !contains(got, "brand_new_column") {
		t.Errorf("columns = %v, want reflection to decide", got)
	}
}

// --- extra field types ------------------------------------------------------

func TestExtraFieldTypes(t *testing.T) {
	p := resolve(t, `
defaults:
  extra_fields:
    tenant_id: acme
    ingested_at: "${now}"
    lag_ms: {value: 0, type: bigint}
`)
	types := map[string]pgtype.PGType{}
	for _, e := range table(t, p, "tasks").ExtraFields {
		types[e.Name] = e.ResolvedType()
	}
	if types["tenant_id"] != pgtype.Text {
		t.Errorf("tenant_id = %q, want text", types["tenant_id"])
	}
	if types["ingested_at"] != pgtype.TimestampTZ {
		t.Errorf("ingested_at = %q, want timestamptz", types["ingested_at"])
	}
	if types["lag_ms"] != pgtype.BigInt {
		t.Errorf("lag_ms = %q, want the declared bigint", types["lag_ms"])
	}
}

// Inferring bigint from "123" would make a column's type depend on whichever
// value someone happened to write first, and change under them later.
func TestNumericLookingLiteralStaysText(t *testing.T) {
	p := resolve(t, "defaults:\n  extra_fields: {build: 123}\n")
	for _, e := range table(t, p, "tasks").ExtraFields {
		if e.Name == "build" && e.ResolvedType() != pgtype.Text {
			t.Errorf("build = %q, want text unless declared", e.ResolvedType())
		}
	}
}

func extras(t Table) map[string]string {
	out := map[string]string{}
	for _, e := range t.ExtraFields {
		out[e.Name] = e.Value
	}
	return out
}

// --- since / until window ---------------------------------------------------

// A literal global filter on created_at is impossible: only some Apiary tables
// have that column. `since` resolves through the catalog to whatever each table
// calls its time dimension.
func TestSinceResolvesToEachTablesOwnTimeColumn(t *testing.T) {
	p := resolve(t, "defaults:\n  since: \"2026-01-01\"\n")
	want := map[string]string{
		"task_executions":      "created_at",
		"execution_events":     "timestamp",
		"ci_poll_checks":       "checked_at",
		"step_runs":            "started_at",
		"worker_registrations": "registered_at",
		"agents":               "updated_at",
	}
	for name, col := range want {
		tbl := table(t, p, name)
		if len(tbl.Filters) != 1 {
			t.Errorf("%s: filters = %v, want one window filter", name, tbl.Filters)
			continue
		}
		if tbl.Filters[0].Column != col {
			t.Errorf("%s: window on %q, want %q", name, tbl.Filters[0].Column, col)
		}
		if !tbl.Filters[0].OrNull {
			t.Errorf("%s: window must keep rows with an unknown timestamp", name)
		}
	}
}

// A step_run that has not started has a NULL started_at and is the most current
// row there is. Dropping it because its timestamp is unknown would discard
// exactly the work in flight.
func TestWindowKeepsRowsWithAnUnknownTimestamp(t *testing.T) {
	p := resolve(t, "defaults:\n  since: \"2026-01-01\"\n")
	f := table(t, p, "step_runs").Filters[0]
	if !strings.Contains(f.String(), "or null") {
		t.Errorf("window filter reads %q, want it to admit nulls", f)
	}
}

// A table with no time dimension replicates in full. That must be reported, not
// implied — it is how a "last 30 days" backfill quietly becomes a full one.
func TestUnwindowedTablesAreNamed(t *testing.T) {
	p := resolve(t, "defaults:\n  since: \"2026-01-01\"\n")
	if len(p.Unwindowed) == 0 {
		t.Fatal("tables with no time column must be named")
	}
	for _, name := range p.Unwindowed {
		if tbl := table(t, p, name); len(tbl.Filters) != 0 {
			t.Errorf("%s is listed as unwindowed but has filters %v", name, tbl.Filters)
		}
	}
	if !contains(p.Unwindowed, "pr_event_watermarks") {
		t.Errorf("Unwindowed = %v, want pr_event_watermarks among them", p.Unwindowed)
	}
}

func TestWindowCombinesWithExplicitFilters(t *testing.T) {
	p := resolve(t, `
defaults:
  since: "2026-01-01"
tables:
  task_executions:
    filters: [{column: status, op: in, value: [success, failed]}]
`)
	got := table(t, p, "task_executions").Filters
	if len(got) != 2 {
		t.Fatalf("filters = %v, want the window and the table filter", got)
	}
	if got[0].Column != "created_at" || got[1].Column != "status" {
		t.Errorf("filters = %v, want window first", got)
	}
}

func TestInvertedWindowIsRejected(t *testing.T) {
	_, err := Parse([]byte(minimal + "defaults:\n  since: \"2026-06-01\"\n  until: \"2026-01-01\"\n"))
	if err == nil {
		t.Fatal("a window that matches no rows must be rejected")
	}
}
