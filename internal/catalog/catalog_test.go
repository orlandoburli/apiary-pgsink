package catalog

import (
	"strings"
	"testing"
)

func load(t *testing.T) *Catalog {
	t.Helper()
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func TestEmbeddedCatalogIsValid(t *testing.T) {
	c := load(t)
	if len(c.Tables) == 0 {
		t.Fatal("catalog is empty")
	}
	if errs := c.Validate(); len(errs) > 0 {
		t.Fatalf("embedded catalog is invalid: %s", joinErrs(errs))
	}
}

// Every class carries obligations. Validate enforces them; this pins that the
// shipped catalog actually satisfies them table by table, so a future edit that
// drops a cursor or a terminal-state list fails here rather than in production.
func TestClassObligations(t *testing.T) {
	for _, tbl := range load(t).Tables {
		switch tbl.Class {
		case ClassAppendOnly, ClassMutable:
			if tbl.Cursor == nil {
				t.Errorf("%s: %s needs a cursor", tbl.Name, tbl.Class)
			}
		case ClassOpenRow:
			if tbl.Cursor == nil || tbl.State == nil || len(tbl.State.Terminal) == 0 {
				t.Errorf("%s: open_row needs a cursor and a non-empty terminal state list", tbl.Name)
			}
		case ClassFollowParent:
			if tbl.Parent == nil || tbl.Cursor != nil {
				t.Errorf("%s: follow_parent needs a parent and no cursor", tbl.Name)
			}
		case ClassSnapshot:
			if tbl.Cursor != nil {
				t.Errorf("%s: snapshot must not declare a cursor", tbl.Name)
			}
		default:
			t.Errorf("%s: unknown class %q", tbl.Name, tbl.Class)
		}
		if len(tbl.Key) == 0 {
			t.Errorf("%s: no key", tbl.Name)
		}
	}
}

// The two tables the whole open_row class exists for. If either is ever
// reclassified as append_only, cost and token totals downstream silently become
// zero — the row is replicated at dispatch and never updated again.
func TestWriteTwiceTablesAreOpenRow(t *testing.T) {
	c := load(t)
	for _, name := range []string{"task_executions", "step_runs"} {
		tbl, ok := c.Table(name)
		if !ok {
			t.Fatalf("%s is not catalogued", name)
		}
		if tbl.Class != ClassOpenRow {
			t.Errorf("%s: class is %q, want open_row — this table is written at "+
				"dispatch and updated at completion, so a cursor alone misses the cost columns",
				name, tbl.Class)
		}
	}
}

// An escalated approval is still awaiting a decision. Treating it as settled
// would freeze the row at its pre-decision state.
func TestEscalatedApprovalIsNotTerminal(t *testing.T) {
	tbl, ok := load(t).Table("approval_requests")
	if !ok {
		t.Fatal("approval_requests is not catalogued")
	}
	if tbl.State.IsTerminal("escalated") {
		t.Error("escalated must not be terminal — the request is still open")
	}
	for _, want := range []string{"approved", "rejected", "timed_out"} {
		if !tbl.State.IsTerminal(want) {
			t.Errorf("%q should be terminal", want)
		}
	}
}

// A row with no state yet is open, not settled.
func TestEmptyStateIsNeverTerminal(t *testing.T) {
	s := State{Column: "state", Terminal: []string{"done"}}
	if s.IsTerminal("") {
		t.Error("empty state must not be terminal")
	}
}

func TestParseRejectsBadCatalogs(t *testing.T) {
	cases := map[string]string{
		"unknown class": `
schema_version: 1
apiary_compat: ">=0.1.0"
tables:
  - {name: t, class: telepathy, key: [id]}`,
		"open_row without state": `
schema_version: 1
apiary_compat: ">=0.1.0"
tables:
  - {name: t, class: open_row, key: [id], cursor: {column: id, kind: integer}}`,
		"open_row with empty terminal list": `
schema_version: 1
apiary_compat: ">=0.1.0"
tables:
  - name: t
    class: open_row
    key: [id]
    cursor: {column: id, kind: integer}
    state: {column: state, terminal: []}`,
		"follow_parent with a cursor": `
schema_version: 1
apiary_compat: ">=0.1.0"
tables:
  - name: t
    class: follow_parent
    key: [id]
    cursor: {column: id, kind: integer}
    parent: {table: p, local: p_id, remote: id}
  - {name: p, class: snapshot, key: [id]}`,
		"parent that does not exist": `
schema_version: 1
apiary_compat: ">=0.1.0"
tables:
  - name: t
    class: follow_parent
    key: [id]
    parent: {table: nowhere, local: p_id, remote: id}`,
		"missing key": `
schema_version: 1
apiary_compat: ">=0.1.0"
tables:
  - {name: t, class: snapshot}`,
		"unknown field": `
schema_version: 1
apiary_compat: ">=0.1.0"
tables:
  - {name: t, class: snapshot, key: [id], colour: blue}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(strings.TrimSpace(raw))); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}
