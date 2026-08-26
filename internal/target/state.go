package target

import (
	"context"
	"fmt"
)

// Watermark is how far one table has been replicated.
type Watermark struct {
	Table  string
	Kind   string
	Value  string
	Rows   int64
	Exists bool
}

// Watermarks reads every recorded position for one Apiary instance.
func (d *DB) Watermarks(ctx context.Context, instance string) (map[string]Watermark, error) {
	rows, err := d.pool.Query(ctx, fmt.Sprintf(
		`SELECT table_name, cursor_kind, COALESCE(cursor_value,''), rows_total
		 FROM %s.%s WHERE %s = $1`, d.schema, StateTable, InstanceColumn), instance)
	if err != nil {
		return nil, fmt.Errorf("read watermarks: %w", err)
	}
	defer rows.Close()
	out := map[string]Watermark{}
	for rows.Next() {
		var w Watermark
		if err := rows.Scan(&w.Table, &w.Kind, &w.Value, &w.Rows); err != nil {
			return nil, err
		}
		w.Exists = true
		out[w.Table] = w
	}
	return out, rows.Err()
}

// SetWatermark records a table's position, adding rowsAdded to its running
// total.
func (d *DB) SetWatermark(ctx context.Context, instance string, w Watermark, rowsAdded int64) error {
	var value any
	if w.Value != "" {
		value = w.Value
	}
	_, err := d.pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.%s (%s, table_name, cursor_kind, cursor_value, rows_total, updated_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (%s, table_name) DO UPDATE SET
		  cursor_kind = excluded.cursor_kind,
		  cursor_value = excluded.cursor_value,
		  rows_total = %s.%s.rows_total + $5,
		  updated_at = now()`,
		d.schema, StateTable, InstanceColumn, InstanceColumn, d.schema, StateTable),
		instance, w.Table, w.Kind, value, rowsAdded)
	if err != nil {
		return fmt.Errorf("set watermark for %s: %w", w.Table, err)
	}
	return nil
}

// OpenKeys returns the keys of rows this target still holds in a non-terminal
// state, most recently written first.
//
// This drives the open_row rescan, and it has to be the *target's* view rather
// than the source's. A row that completes between two passes is no longer open
// in the source, and its cursor has not moved — task_executions is keyed on an
// autoincrement id and carries no updated_at — so asking the source "what is
// unsettled now?" misses precisely the completion the class exists to capture.
// Asking the target "what did I last record as unsettled?" cannot: the row is
// in that set until the pass that settles it.
//
// The set is bounded by how much work Apiary can have in flight, so it is
// normally a handful of rows. limit caps it anyway, so a target left full of
// interrupted rows degrades to slower convergence rather than an unbounded
// query.
func (d *DB) OpenKeys(ctx context.Context, instance, table, keyColumn, stateColumn string, terminal []string, limit int) ([]any, error) {
	args := []any{instance}
	placeholders := ""
	for i, v := range terminal {
		if i > 0 {
			placeholders += ", "
		}
		placeholders += fmt.Sprintf("$%d", len(args)+1)
		args = append(args, v)
	}
	if placeholders == "" {
		placeholders = "NULL"
	}
	query := fmt.Sprintf(
		`SELECT %s FROM %s.%s
		 WHERE %s = $1 AND (%s IS NULL OR %s NOT IN (%s))
		 LIMIT %d`,
		keyColumn, d.schema, table, InstanceColumn, stateColumn, stateColumn, placeholders, limit)

	rows, err := d.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read open rows of %s: %w", table, err)
	}
	defer rows.Close()
	var out []any
	for rows.Next() {
		var key any
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, rows.Err()
}
