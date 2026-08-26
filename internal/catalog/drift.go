package catalog

import (
	"fmt"
	"sort"
	"strings"
)

// LiveColumn is one column as it exists in a running Apiary database.
type LiveColumn struct {
	Name string
	Type string
}

// LiveTable is one table as it exists in a running Apiary database.
type LiveTable struct {
	Name       string
	Columns    []LiveColumn
	PrimaryKey []string
}

// Has reports whether the table has a column of that name.
func (t LiveTable) Has(column string) bool {
	for _, c := range t.Columns {
		if c.Name == column {
			return true
		}
	}
	return false
}

// LiveSchema is a reflected Apiary database, keyed by table name. Producing one
// is the job of a source reader; this package only compares against it.
type LiveSchema map[string]LiveTable

// Severity separates "this will produce wrong data" from "you may be missing
// something".
type Severity string

const (
	// Error means following the table as catalogued would be incorrect.
	Error Severity = "error"
	// Warn means something changed that is worth a look but is still safe.
	Warn Severity = "warn"
)

// Finding is one difference between the catalog and a live schema.
type Finding struct {
	Severity Severity
	Table    string
	Message  string
}

func (f Finding) String() string {
	return fmt.Sprintf("%-5s %-28s %s", f.Severity, f.Table, f.Message)
}

// Drift compares the catalog against a live Apiary schema.
//
// Note what is deliberately absent: new columns are not a finding. pgsink
// replicates whatever columns reflection reports, so an Apiary release that
// adds a column is picked up without any change here. Only columns the catalog
// names — cursors, state, keys, parent links — can break, and those are exactly
// what this checks.
func Drift(c *Catalog, live LiveSchema) []Finding {
	var out []Finding
	add := func(s Severity, table, format string, args ...any) {
		out = append(out, Finding{Severity: s, Table: table, Message: fmt.Sprintf(format, args...)})
	}

	for _, t := range c.Tables {
		lt, ok := live[t.Name]
		if !ok {
			// A warning, not an error. pgsink supports a range of Apiary
			// versions, and an older database legitimately lacks tables a newer
			// one has — improvement_runs and improvement_findings arrived in a
			// migration, so any database predating it is missing them. Skipping
			// the table is correct. This becomes an error only when the table is
			// explicitly enabled in configuration, which is the caller's check,
			// not this one's.
			add(Warn, t.Name, "catalogued but not in this database; it will be skipped "+
				"(expected on older Apiary versions)")
			continue
		}
		for _, k := range t.Key {
			if !lt.Has(k) {
				add(Error, t.Name, "key column %q is gone — there is no conflict target to upsert on", k)
			}
		}
		if len(lt.PrimaryKey) > 0 && !sameSet(t.Key, lt.PrimaryKey) {
			add(Warn, t.Name, "catalog key [%s] differs from the live primary key [%s]",
				strings.Join(t.Key, ", "), strings.Join(lt.PrimaryKey, ", "))
		}
		if t.Cursor != nil && !lt.Has(t.Cursor.Column) {
			add(Error, t.Name, "cursor column %q is gone — the table cannot be followed", t.Cursor.Column)
		}
		if t.State != nil && !lt.Has(t.State.Column) {
			severity := Warn
			if t.Class == ClassOpenRow {
				// Without the state column the rescan has nothing to bound it,
				// and updates to settled rows would be missed entirely.
				severity = Error
			}
			add(severity, t.Name, "state column %q is gone", t.State.Column)
		}
		if t.Parent != nil {
			if !lt.Has(t.Parent.Local) {
				add(Error, t.Name, "parent link column %q is gone", t.Parent.Local)
			}
			if pt, ok := live[t.Parent.Table]; !ok {
				add(Error, t.Name, "parent table %q is missing from the database", t.Parent.Table)
			} else if !pt.Has(t.Parent.Remote) {
				add(Error, t.Name, "parent column %s.%s is gone", t.Parent.Table, t.Parent.Remote)
			}
		}
		for _, col := range t.Timestamps {
			if !lt.Has(col) {
				add(Warn, t.Name, "declared timestamp column %q is not in this database", col)
			}
		}
		for _, col := range t.JSONColumns {
			if !lt.Has(col) {
				add(Warn, t.Name, "declared json column %q is not in this database", col)
			}
		}
		for _, col := range t.LargeColumns {
			if !lt.Has(col) {
				add(Warn, t.Name, "declared large column %q is not in this database", col)
			}
		}
	}

	for _, name := range sortedKeys(live) {
		if _, ok := c.Table(name); !ok {
			add(Warn, name, "present in the database but not catalogued; it will not be replicated")
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity == Error
		}
		return out[i].Table < out[j].Table
	})
	return out
}

// HasErrors reports whether any finding would make replication incorrect.
func HasErrors(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity == Error {
			return true
		}
	}
	return false
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func sortedKeys(s LiveSchema) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
