package config

import (
	"fmt"
	"sort"

	"github.com/orlandoburli/apiary-pgsink/internal/catalog"
)

// ExtraField is one injected column after resolution, carrying the name so the
// set can be ordered deterministically.
type ExtraField struct {
	Name string
	Extra
}

// Table is one table's fully resolved plan.
type Table struct {
	Name    string
	Catalog *catalog.Table

	// Include, when non-empty, is the exact projection. Exclude is applied
	// afterwards, so a column in both is dropped.
	Include []string
	Exclude map[string]bool

	ExtraFields []ExtraField
	Filters     []Filter
	JSONColumns []string
}

// Windowed reports whether a `since`/`until` window applied to this table.
func (t Table) Windowed() bool { return t.Catalog.TimeColumn != "" }

// Plan is the resolved configuration: what to read, from where, to where.
type Plan struct {
	Source Source
	Target Target
	Sync   Sync
	Tables []Table
	// Unwindowed names the enabled tables that defaults.since could not apply
	// to because they have no time dimension. They replicate in full. Reported
	// rather than assumed — a silently unwindowed table is how a "last 30 days"
	// backfill quietly becomes a full one.
	Unwindowed []string
}

// Resolve combines the global defaults with each table's settings and drops
// everything disabled. The result is ordered by table name so runs are
// reproducible and diffs are readable.
//
// The merge rules are fixed, and three of the four are deliberately not
// symmetric:
//
//   - extra_fields  shallow merge, table key wins. Injected metadata is per-row
//     decoration, and a table naturally refines it.
//   - exclude_columns  union, never subtractive. A global exclusion is usually a
//     PII or volume guarantee; a table block must not quietly re-admit a column.
//   - include_columns  table-only. There is no global form: no two Apiary tables
//     share a column set, so a global projection would omit some table's key or
//     cursor. Rejected at validation with that explanation.
//   - filters  AND, never override. Same reasoning as exclusions: global filters
//     are guarantees. IgnoreGlobalFilters is the only escape, and it is loud.
func Resolve(f *File, cat *catalog.Catalog) (*Plan, []error) {
	var errs []error
	plan := &Plan{Source: f.Source, Target: f.Target, Sync: f.Sync}

	// A table configured but not catalogued is almost always a typo, and a
	// silent one: the block would simply never apply.
	for _, name := range sortedTableNames(f.Tables) {
		if _, ok := cat.Table(name); !ok {
			errs = append(errs, fmt.Errorf("tables.%s: not a known Apiary table; known tables: %d in the catalog, run `pgsink doctor` to list them", name, len(cat.Tables)))
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}

	for _, ct := range cat.Tables {
		tc, configured := f.Tables[ct.Name]
		if !enabled(f.Defaults.Enabled, tc.Enabled, configured) {
			continue
		}
		entry := ct
		rt := Table{
			Name:        ct.Name,
			Catalog:     &entry,
			Include:     append([]string(nil), tc.IncludeColumns...),
			Exclude:     unionColumns(f.Defaults.ExcludeColumns, tc.ExcludeColumns),
			ExtraFields: mergeExtras(f.Defaults.ExtraFields, tc.ExtraFields),
			Filters:     andFilters(windowFilters(ct, f.Defaults), f.Defaults.Filters, tc.Filters, tc.IgnoreGlobalFilters),
			JSONColumns: mergeJSON(ct.JSONColumns, tc.JSONColumns),
		}
		// Dropping a key column makes the row un-upsertable: there would be no
		// conflict target, and rows would duplicate on every re-delivery.
		for _, k := range ct.Key {
			if rt.Exclude[k] {
				errs = append(errs, fmt.Errorf("tables.%s: key column %q cannot be excluded; it is the upsert conflict target", ct.Name, k))
			}
			if len(rt.Include) > 0 && !contains(rt.Include, k) {
				errs = append(errs, fmt.Errorf("tables.%s: include_columns omits key column %q; it is the upsert conflict target", ct.Name, k))
			}
		}
		// The same for the cursor: without it the table cannot advance, and
		// would be re-read from zero forever.
		if ct.Cursor != nil {
			if rt.Exclude[ct.Cursor.Column] {
				errs = append(errs, fmt.Errorf("tables.%s: cursor column %q cannot be excluded; the table could not advance without it", ct.Name, ct.Cursor.Column))
			}
			if len(rt.Include) > 0 && !contains(rt.Include, ct.Cursor.Column) {
				errs = append(errs, fmt.Errorf("tables.%s: include_columns omits cursor column %q", ct.Name, ct.Cursor.Column))
			}
		}
		if (f.Defaults.Since != "" || f.Defaults.Until != "") && ct.TimeColumn == "" {
			plan.Unwindowed = append(plan.Unwindowed, ct.Name)
		}
		plan.Tables = append(plan.Tables, rt)
	}

	if len(plan.Tables) == 0 {
		errs = append(errs, fmt.Errorf("no tables are enabled; nothing would be replicated"))
	}
	sort.Slice(plan.Tables, func(i, j int) bool { return plan.Tables[i].Name < plan.Tables[j].Name })
	return plan, errs
}

// enabled resolves the three-state on/off: an explicit table setting wins, then
// the global default.
func enabled(global, table *bool, configured bool) bool {
	if configured && table != nil {
		return *table
	}
	return global == nil || *global
}

func unionColumns(global, table []string) map[string]bool {
	out := make(map[string]bool, len(global)+len(table))
	for _, c := range global {
		out[c] = true
	}
	for _, c := range table {
		out[c] = true
	}
	return out
}

func mergeExtras(global, table map[string]Extra) []ExtraField {
	merged := make(map[string]Extra, len(global)+len(table))
	for k, v := range global {
		merged[k] = v
	}
	for k, v := range table {
		merged[k] = v
	}
	names := make([]string, 0, len(merged))
	for k := range merged {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]ExtraField, 0, len(names))
	for _, n := range names {
		out = append(out, ExtraField{Name: n, Extra: merged[n]})
	}
	return out
}

func andFilters(window, global, table []Filter, ignoreGlobal bool) []Filter {
	out := make([]Filter, 0, len(window)+len(global)+len(table))
	if !ignoreGlobal {
		out = append(out, window...)
		out = append(out, global...)
	}
	return append(out, table...)
}

// windowFilters turns defaults.since/until into filters on this table's own
// time column.
//
// The comparison is deliberately null-tolerant. A step_run that has not started
// has a NULL started_at, and it is the most current row there is — excluding it
// because its timestamp is unknown would drop exactly the work in flight.
func windowFilters(ct catalog.Table, d Defaults) []Filter {
	if ct.TimeColumn == "" {
		return nil
	}
	var out []Filter
	if d.Since != "" {
		out = append(out, Filter{Column: ct.TimeColumn, Op: OpGte, Value: d.Since, OrNull: true})
	}
	if d.Until != "" {
		out = append(out, Filter{Column: ct.TimeColumn, Op: OpLt, Value: d.Until, OrNull: true})
	}
	return out
}

// mergeJSON unions the catalog's known JSON columns with any the operator adds.
// The catalog is right about Apiary's own schema; configuration can only extend
// it, never contradict it.
func mergeJSON(fromCatalog, fromConfig []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range append(append([]string(nil), fromCatalog...), fromConfig...) {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Columns returns the source columns to read for this table, given the live
// schema, after projection and exclusion.
func (t Table) Columns(live catalog.LiveTable) []string {
	var out []string
	if len(t.Include) > 0 {
		for _, c := range t.Include {
			if live.Has(c) && !t.Exclude[c] {
				out = append(out, c)
			}
		}
		return out
	}
	for _, c := range live.Columns {
		if !t.Exclude[c.Name] {
			out = append(out, c.Name)
		}
	}
	return out
}

// Check validates the plan against a reflected schema — everything that needs
// real column names rather than just the shape of the document.
func (p *Plan) Check(live catalog.LiveSchema) []error {
	var errs []error
	for _, t := range p.Tables {
		lt, ok := live[t.Name]
		if !ok {
			// Not an error: an older Apiary legitimately lacks newer tables,
			// and the table is simply skipped. Drift reporting says so.
			continue
		}
		for _, f := range t.Filters {
			if !lt.Has(f.Column) {
				errs = append(errs, fmt.Errorf("tables.%s: filter on %q, which this database's %s does not have", t.Name, f.Column, t.Name))
			}
		}
		for _, c := range t.Include {
			if !lt.Has(c) {
				errs = append(errs, fmt.Errorf("tables.%s: include_columns names %q, which this database's %s does not have", t.Name, c, t.Name))
			}
		}
		for _, e := range t.ExtraFields {
			// An extra field that shadows a source column would be written
			// twice into one target column, with the injected value winning
			// silently.
			if lt.Has(e.Name) {
				errs = append(errs, fmt.Errorf("tables.%s: extra field %q collides with a real column of the same name", t.Name, e.Name))
			}
			for _, ref := range e.References() {
				if !lt.Has(ref) {
					errs = append(errs, fmt.Errorf("tables.%s: extra field %q reads ${row.%s}, which does not exist", t.Name, e.Name, ref))
				}
				if t.Exclude[ref] {
					errs = append(errs, fmt.Errorf("tables.%s: extra field %q reads ${row.%s}, which is excluded — it would never be read", t.Name, e.Name, ref))
				}
			}
		}
	}
	return errs
}
