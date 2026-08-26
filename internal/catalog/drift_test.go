package catalog

import "testing"

func mustLoad(t *testing.T) *Catalog {
	t.Helper()
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

// pgsink supports a range of Apiary versions. A database predating the
// migration that created improvement_runs is not broken — that table is simply
// skipped. Treating it as an error would refuse every older installation.
func TestMissingTableIsAWarningNotAnError(t *testing.T) {
	c := mustLoad(t)
	live := LiveSchema{}
	for _, tbl := range c.Tables {
		if tbl.Name == "improvement_runs" || tbl.Name == "improvement_findings" {
			continue
		}
		live[tbl.Name] = syntheticTable(tbl)
	}
	findings := Drift(c, live)
	if HasErrors(findings) {
		t.Fatalf("a missing table must not be an error: %v", findings)
	}
	var saw bool
	for _, f := range findings {
		if f.Table == "improvement_runs" && f.Severity == Warn {
			saw = true
		}
	}
	if !saw {
		t.Error("expected a warning naming improvement_runs")
	}
}

// A cursor column that disappears silently stops the table advancing, so it has
// to be loud.
func TestMissingCursorColumnIsAnError(t *testing.T) {
	c := mustLoad(t)
	live := LiveSchema{}
	for _, tbl := range c.Tables {
		lt := syntheticTable(tbl)
		if tbl.Name == "tasks" {
			lt.Columns = dropColumn(lt.Columns, "updated_at")
		}
		live[tbl.Name] = lt
	}
	if !HasErrors(Drift(c, live)) {
		t.Fatal("a missing cursor column must be an error")
	}
}

// Without its state column an open_row table cannot bound its rescan, so
// updates to already-replicated rows would be lost.
func TestMissingStateColumnIsAnErrorForOpenRow(t *testing.T) {
	c := mustLoad(t)
	live := LiveSchema{}
	for _, tbl := range c.Tables {
		lt := syntheticTable(tbl)
		if tbl.Name == "task_executions" {
			lt.Columns = dropColumn(lt.Columns, "status")
		}
		live[tbl.Name] = lt
	}
	if !HasErrors(Drift(c, live)) {
		t.Fatal("a missing state column on an open_row table must be an error")
	}
}

func TestUncataloguedTableIsAWarning(t *testing.T) {
	c := mustLoad(t)
	live := LiveSchema{}
	for _, tbl := range c.Tables {
		live[tbl.Name] = syntheticTable(tbl)
	}
	live["brand_new_table"] = LiveTable{Name: "brand_new_table", Columns: []LiveColumn{{Name: "id", Type: "TEXT"}}}
	findings := Drift(c, live)
	if HasErrors(findings) {
		t.Fatalf("an uncatalogued table must not be an error: %v", findings)
	}
	for _, f := range findings {
		if f.Table == "brand_new_table" {
			return
		}
	}
	t.Error("expected a warning naming brand_new_table")
}

// syntheticTable builds a live table carrying exactly the columns the catalog
// entry names, so a test can remove one and see what Drift makes of it.
func syntheticTable(t Table) LiveTable {
	seen := map[string]bool{}
	lt := LiveTable{Name: t.Name, PrimaryKey: append([]string(nil), t.Key...)}
	addCol := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		lt.Columns = append(lt.Columns, LiveColumn{Name: name, Type: "TEXT"})
	}
	for _, k := range t.Key {
		addCol(k)
	}
	if t.Cursor != nil {
		addCol(t.Cursor.Column)
	}
	if t.State != nil {
		addCol(t.State.Column)
	}
	if t.Parent != nil {
		addCol(t.Parent.Local)
	}
	for _, c := range t.Timestamps {
		addCol(c)
	}
	for _, c := range t.JSONColumns {
		addCol(c)
	}
	for _, c := range t.LargeColumns {
		addCol(c)
	}
	return lt
}

func dropColumn(cols []LiveColumn, name string) []LiveColumn {
	out := cols[:0:0]
	for _, c := range cols {
		if c.Name != name {
			out = append(out, c)
		}
	}
	return out
}
