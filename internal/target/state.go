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
