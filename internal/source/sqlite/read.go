package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/orlandoburli/apiary-pgsink/internal/config"
)

// Selector is an extra predicate added to a read, on top of the table's
// configured filters. Sync uses it to express what "changed" means for each
// catalog class; an empty Selector selects everything.
type Selector struct {
	SQL  string
	Args []any
}

// WatermarkLayout is how a normalised timestamp watermark is written. It
// matches strftime('%Y-%m-%d %H:%M:%f', …), which renders UTC to milliseconds.
const WatermarkLayout = "2006-01-02 15:04:05.000"

// Page is one batch of rows read from a table.
type Page struct {
	Columns []string
	Rows    [][]any
	// LastRowID is the highest rowid in this page, and the cursor for the next
	// one. Zero when the page is empty.
	LastRowID int64
}

// Batch reads up to limit rows after a rowid, applying the table's filters.
//
// Paging is by rowid rather than by the table's own key. Every Apiary table is
// a rowid table, so this gives a single, always-present, monotonic, indexed
// column to page on — including for the two tables with composite keys and the
// ones whose key is a ulid. It is also stable under concurrent writes in a way
// OFFSET is not: a row inserted behind the cursor cannot shift a later page.
func Batch(ctx context.Context, db *sql.DB, table string, columns []string, filters []config.Filter, afterRowID int64, limit int, extra ...Selector) (*Page, error) {
	if len(columns) == 0 {
		return nil, fmt.Errorf("table %s: no columns selected", table)
	}
	where, args := Compile(filters)
	clause := "rowid > ?"
	if where != "" {
		clause += " AND " + where
	}
	for _, sel := range extra {
		if sel.SQL == "" {
			continue
		}
		clause += " AND " + sel.SQL
		args = append(args, sel.Args...)
	}
	query := fmt.Sprintf("SELECT rowid, %s FROM %s WHERE %s ORDER BY rowid LIMIT ?",
		strings.Join(columns, ", "), table, clause)

	rows, err := db.QueryContext(ctx, query, append(append([]any{afterRowID}, args...), limit)...)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", table, err)
	}
	defer rows.Close()

	page := &Page{Columns: columns}
	for rows.Next() {
		values := make([]any, len(columns)+1)
		scan := make([]any, len(values))
		for i := range values {
			scan[i] = &values[i]
		}
		if err := rows.Scan(scan...); err != nil {
			return nil, fmt.Errorf("scan %s: %w", table, err)
		}
		rowID, _ := values[0].(int64)
		page.LastRowID = rowID
		page.Rows = append(page.Rows, values[1:])
	}
	return page, rows.Err()
}

// Compile turns filters into a SQL fragment and its arguments.
//
// Filters run here, inside the read, not after transfer: a row the filter
// rejects is never read, never serialised and never sent. Values are always
// bound as parameters — a filter's value comes from a configuration file, and
// interpolating it would be an injection point even if the column name beside
// it is validated as an identifier.
func Compile(filters []config.Filter) (string, []any) {
	var parts []string
	var args []any
	for _, f := range filters {
		switch f.Op {
		case config.OpIsNull:
			parts = append(parts, fmt.Sprintf("%s IS NULL", f.Column))
			continue
		case config.OpNotNull:
			parts = append(parts, fmt.Sprintf("%s IS NOT NULL", f.Column))
			continue
		case config.OpIn, config.OpNotIn:
			list, _ := f.Value.([]any)
			holders := make([]string, len(list))
			for i, v := range list {
				holders[i] = "?"
				args = append(args, v)
			}
			op := "IN"
			if f.Op == config.OpNotIn {
				op = "NOT IN"
			}
			parts = append(parts, wrapNull(f, fmt.Sprintf("%s %s (%s)", f.Column, op, strings.Join(holders, ", "))))
			continue
		}
		parts = append(parts, wrapNull(f, fmt.Sprintf("%s %s ?", f.Column, sqlOp(f.Op))))
		args = append(args, f.Value)
	}
	return strings.Join(parts, " AND "), args
}

// wrapNull widens a comparison to keep rows whose value is NULL, for the
// since/until window. SQL's three-valued logic makes `started_at >= x` false
// for a NULL, which would drop precisely the rows that have not started yet.
func wrapNull(f config.Filter, expr string) string {
	if !f.OrNull {
		return expr
	}
	return fmt.Sprintf("(%s OR %s IS NULL)", expr, f.Column)
}

func sqlOp(op config.Op) string {
	switch op {
	case config.OpEq:
		return "="
	case config.OpNe:
		return "!="
	case config.OpLt:
		return "<"
	case config.OpLte:
		return "<="
	case config.OpGt:
		return ">"
	case config.OpGte:
		return ">="
	case config.OpLike:
		return "LIKE"
	default:
		return "="
	}
}

// CountRows returns how many rows a table holds under the given filters, for
// progress reporting.
func CountRows(ctx context.Context, db *sql.DB, table string, filters []config.Filter) (int64, error) {
	where, args := Compile(filters)
	query := "SELECT COUNT(*) FROM " + table
	if where != "" {
		query += " WHERE " + where
	}
	var n int64
	if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s: %w", table, err)
	}
	return n, nil
}
