// Package target writes to PostgreSQL.
package target

import (
	"fmt"
	"sort"
	"strings"

	"github.com/orlandoburli/apiary-pgsink/internal/catalog"
	"github.com/orlandoburli/apiary-pgsink/internal/config"
	"github.com/orlandoburli/apiary-pgsink/internal/pgtype"
)

// InstanceColumn is added to every replicated table and forms the first part of
// its primary key.
//
// It is structural, not an extra field. Apiary's ids are unique within one
// installation but nothing stops two installations minting the same ulid or
// autoincrement id, so a target that can hold more than one Apiary needs the
// instance in the key. Making it automatic means a second source can never
// silently overwrite the first because someone forgot to configure it.
const InstanceColumn = "apiary_instance"

// StateTable records how far each table has been replicated. It lives in the
// target so the sink itself is stateless: move it to another host and it
// resumes from where the data actually is.
const StateTable = "apiary_sync_state"

// Column is one column of a target table.
type Column struct {
	Name string
	Type pgtype.PGType
	// Source is the SQLite column this comes from. Empty for injected columns.
	Source string
}

// TableSchema is the target shape of one replicated table.
type TableSchema struct {
	Name    string
	Columns []Column
	Key     []string
}

// Desired computes the target schema for one planned table against a live
// Apiary schema.
//
// Ordering is deterministic — instance column, then source columns in the order
// SQLite reports them, then extra fields alphabetically — so generated DDL is
// stable across runs and reviewable in a diff.
func Desired(t config.Table, live catalog.LiveTable) (TableSchema, error) {
	json := map[string]bool{}
	for _, c := range t.JSONColumns {
		json[c] = true
	}
	types := map[string]string{}
	for _, c := range live.Columns {
		types[c.Name] = c.Type
	}

	out := TableSchema{
		Name:    t.Name,
		Columns: []Column{{Name: InstanceColumn, Type: pgtype.Text}},
		Key:     append([]string{InstanceColumn}, t.Catalog.Key...),
	}
	for _, name := range t.Columns(live) {
		out.Columns = append(out.Columns, Column{
			Name:   name,
			Type:   pgtype.MapColumn(types[name], json[name]),
			Source: name,
		})
	}

	seen := map[string]bool{}
	for _, c := range out.Columns {
		seen[c.Name] = true
	}
	extras := append([]config.ExtraField(nil), t.ExtraFields...)
	sort.Slice(extras, func(i, j int) bool { return extras[i].Name < extras[j].Name })
	for _, e := range extras {
		if e.Name == InstanceColumn {
			// pgsink writes this itself, from source.instance. An extra field
			// of the same name would fight it for the primary key.
			return out, fmt.Errorf("table %s: %q is reserved — pgsink writes it from source.instance; remove the extra field", t.Name, InstanceColumn)
		}
		if seen[e.Name] {
			return out, fmt.Errorf("table %s: extra field %q collides with a replicated column", t.Name, e.Name)
		}
		seen[e.Name] = true
		out.Columns = append(out.Columns, Column{Name: e.Name, Type: e.ResolvedType()})
	}

	for _, k := range out.Key {
		if !seen[k] {
			return out, fmt.Errorf("table %s: key column %q is not among the replicated columns", t.Name, k)
		}
	}
	return out, nil
}

// CreateSQL renders the CREATE TABLE for this schema.
func (s TableSchema) CreateSQL(schema string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s.%s (\n", schema, s.Name)
	for _, c := range s.Columns {
		fmt.Fprintf(&b, "  %-28s %s,\n", c.Name, c.Type)
	}
	fmt.Fprintf(&b, "  PRIMARY KEY (%s)\n)", strings.Join(s.Key, ", "))
	return b.String()
}

// AddColumnSQL renders the ALTER that adds one column.
func (s TableSchema) AddColumnSQL(schema string, c Column) string {
	return fmt.Sprintf("ALTER TABLE %s.%s ADD COLUMN %s %s", schema, s.Name, c.Name, c.Type)
}

// StateTableSQL renders the watermark table.
func StateTableSQL(schema string) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s (
  %-28s text        NOT NULL,
  %-28s text        NOT NULL,
  %-28s text        NOT NULL,
  %-28s text,
  %-28s bigint      NOT NULL DEFAULT 0,
  %-28s timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (%s, table_name)
)`, schema, StateTable,
		InstanceColumn, "table_name", "cursor_kind", "cursor_value", "rows_total", "updated_at",
		InstanceColumn)
}

// Change is one piece of work `migrate` would do.
type Change struct {
	Table string
	SQL   string
	// Blocking marks a difference migrate will not resolve on its own.
	Blocking bool
	Reason   string
}

// Diff compares a desired schema against what the target already has, and
// returns the changes needed.
//
// Additive only. A column whose type has changed, or one the target has and the
// plan no longer wants, is reported rather than altered or dropped: both are
// destructive, and an operator should decide, not a sync loop.
func Diff(schema string, want TableSchema, have map[string]pgtype.PGType) []Change {
	if len(have) == 0 {
		return []Change{{Table: want.Name, SQL: want.CreateSQL(schema)}}
	}
	var out []Change
	for _, c := range want.Columns {
		existing, ok := have[c.Name]
		if !ok {
			out = append(out, Change{Table: want.Name, SQL: want.AddColumnSQL(schema, c)})
			continue
		}
		if existing != c.Type {
			out = append(out, Change{
				Table:    want.Name,
				Blocking: true,
				Reason: fmt.Sprintf("column %s is %s in the target but %s in the plan; "+
					"changing it may lose data, so pgsink will not do it for you",
					c.Name, existing, c.Type),
			})
		}
	}
	return out
}
