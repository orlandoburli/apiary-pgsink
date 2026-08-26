package target

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/orlandoburli/apiary-pgsink/internal/pgtype"
)

// Writer upserts batches into one target table.
type Writer struct {
	db       *DB
	schema   TableSchema
	instance string

	columns []string
	types   []pgtype.PGType
	// sourceIdx maps each target column to its index in a source row, or -1 for
	// injected columns.
	sourceIdx []int
	sql       string
}

// NewWriter prepares the upsert for one table. sourceColumns is the order rows
// arrive in from the reader.
func NewWriter(db *DB, schema TableSchema, instance string, sourceColumns []string) *Writer {
	index := map[string]int{}
	for i, c := range sourceColumns {
		index[c] = i
	}
	w := &Writer{db: db, schema: schema, instance: instance}
	for _, c := range schema.Columns {
		w.columns = append(w.columns, c.Name)
		w.types = append(w.types, c.Type)
		if c.Source == "" {
			w.sourceIdx = append(w.sourceIdx, -1)
			continue
		}
		if i, ok := index[c.Source]; ok {
			w.sourceIdx = append(w.sourceIdx, i)
		} else {
			w.sourceIdx = append(w.sourceIdx, -1)
		}
	}
	w.sql = w.upsertSQL()
	return w
}

// upsertSQL renders the INSERT ... ON CONFLICT DO UPDATE.
//
// Idempotence is the whole design. Every delivery path — a re-run backfill, a
// cursor overlap window, an open-row rescan, a retry after a crash — can deliver
// the same row again, and must land on the same result. That is what lets the
// pipeline promise at-least-once without also promising duplicates.
func (w *Writer) upsertSQL() string {
	placeholders := make([]string, len(w.columns))
	for i := range w.columns {
		placeholders[i] = "$" + strconv.Itoa(i+1)
	}
	key := map[string]bool{}
	for _, k := range w.schema.Key {
		key[k] = true
	}
	var updates []string
	for _, c := range w.columns {
		if !key[c] {
			updates = append(updates, fmt.Sprintf("%s = excluded.%s", c, c))
		}
	}
	stmt := fmt.Sprintf("INSERT INTO %s.%s (%s) VALUES (%s) ON CONFLICT (%s) ",
		w.db.schema, w.schema.Name,
		strings.Join(w.columns, ", "), strings.Join(placeholders, ", "),
		strings.Join(w.schema.Key, ", "))
	if len(updates) == 0 {
		// A table whose every column is part of the key — pr_event_dispatches
		// nearly is. Re-delivering such a row has nothing to update.
		return stmt + "DO NOTHING"
	}
	return stmt + "DO UPDATE SET " + strings.Join(updates, ", ")
}

// ExtraValue is one injected value for a row, positioned by column name.
type ExtraValue struct {
	Name  string
	Value any
}

// WriteBatch upserts rows in a single transaction, so a batch either lands
// whole or not at all and the watermark can never advance past a partial write.
func (w *Writer) WriteBatch(ctx context.Context, rows [][]any, extras [][]ExtraValue) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	extraIdx := map[string]int{}
	for i, c := range w.columns {
		extraIdx[c] = i
	}

	var written int64
	err := pgx.BeginFunc(ctx, w.db.pool, func(tx pgx.Tx) error {
		batch := &pgx.Batch{}
		for i, row := range rows {
			values := make([]any, len(w.columns))
			for j := range w.columns {
				switch {
				case w.columns[j] == InstanceColumn:
					values[j] = w.instance
				case w.sourceIdx[j] >= 0:
					values[j] = convert(row[w.sourceIdx[j]], w.types[j])
				default:
					values[j] = nil
				}
			}
			if i < len(extras) {
				for _, e := range extras[i] {
					if k, ok := extraIdx[e.Name]; ok {
						values[k] = e.Value
					}
				}
			}
			batch.Queue(w.sql, values...)
		}
		results := tx.SendBatch(ctx, batch)
		defer results.Close()
		for range rows {
			tag, err := results.Exec()
			if err != nil {
				return err
			}
			written += tag.RowsAffected()
		}
		return results.Close()
	})
	if err != nil {
		return 0, fmt.Errorf("write %s: %w", w.schema.Name, err)
	}
	return written, nil
}

// convert adapts a value the SQLite driver produced to what the target column
// needs.
//
// Three cases matter, and all three are places SQLite's weak typing meets
// PostgreSQL's strict typing:
//
//   - boolean. SQLite has no boolean storage class, so a column declared
//     BOOLEAN holds 0 or 1 and comes back as int64.
//   - timestamptz. Apiary writes times as text. Rows written before its
//     _time_format fix carry a Go time.Time.String() suffix — " m=+0.000",
//     a monotonic-clock reading — that no standard parser accepts, and the
//     backfill will meet them in the historical data.
//   - jsonb. Empty text is not valid JSON; NULL is the honest equivalent.
func convert(v any, t pgtype.PGType) any {
	if v == nil {
		return nil
	}
	switch t {
	case pgtype.Boolean:
		switch n := v.(type) {
		case int64:
			return n != 0
		case bool:
			return n
		}
	case pgtype.TimestampTZ:
		if s, ok := asString(v); ok {
			return parseTime(s)
		}
	case pgtype.JSONB:
		if s, ok := asString(v); ok {
			if strings.TrimSpace(s) == "" {
				return nil
			}
			return []byte(s)
		}
	}
	if b, ok := v.([]byte); ok && t == pgtype.Text {
		return string(b)
	}
	return v
}

func asString(v any) (string, bool) {
	switch s := v.(type) {
	case string:
		return s, true
	case []byte:
		return string(s), true
	}
	return "", false
}

// timeLayouts covers every shape Apiary has written a timestamp in.
var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999 -07:00",
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02",
}

func parseTime(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if t, ok := parseKnownLayouts(s); ok {
		return t
	}
	// Legacy rows carry Go's monotonic-clock suffix, which nothing parses.
	// Apiary's own DATE()/datetime() queries silently dropped these rows; the
	// backfill can do better by trimming it rather than discarding the value.
	if i := strings.Index(s, " m=="); i > 0 {
		s = s[:i]
	} else if i := strings.Index(s, " m=+"); i > 0 {
		s = s[:i]
	} else if i := strings.Index(s, " m=-"); i > 0 {
		s = s[:i]
	}
	if t, ok := parseKnownLayouts(strings.TrimSpace(s)); ok {
		return t
	}
	return nil
}

func parseKnownLayouts(s string) (time.Time, bool) {
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// Quarantined reports one row the target refused.
type Quarantined struct {
	Key   string
	Error string
}

// WriteBatchIsolating retries a failed batch one row at a time, quarantining
// the rows that fail on their own.
//
// The fast path stays a single transaction, because that is what makes a batch
// atomic and the watermark safe. Only when it fails does the writer fall back to
// per-row writes, which is slower but bounded to the batch — and is the
// difference between a sink that quarantines one bad value and one that stops
// dead on it.
func (w *Writer) WriteBatchIsolating(ctx context.Context, rows [][]any, extras [][]ExtraValue) (int64, []Quarantined, error) {
	written, err := w.WriteBatch(ctx, rows, extras)
	if err == nil {
		return written, nil, nil
	}
	// A cancelled context is not a poison row — retrying row by row would just
	// fail the same way, slower.
	if ctx.Err() != nil {
		return 0, nil, err
	}

	written = 0
	var bad []Quarantined
	for i, row := range rows {
		var rowExtras [][]ExtraValue
		if i < len(extras) {
			rowExtras = [][]ExtraValue{extras[i]}
		}
		n, rowErr := w.WriteBatch(ctx, [][]any{row}, rowExtras)
		if rowErr == nil {
			written += n
			continue
		}
		if ctx.Err() != nil {
			return written, bad, rowErr
		}
		key, qErr := w.quarantine(ctx, row, rowErr)
		if qErr != nil {
			// The quarantine itself failing means the target is unhealthy, not
			// that the row is bad. Report the original failure and stop.
			return written, bad, fmt.Errorf("%w (and quarantine failed: %v)", rowErr, qErr)
		}
		bad = append(bad, Quarantined{Key: key, Error: rowErr.Error()})
	}
	return written, bad, nil
}

// quarantine records one refused row, returning the key it was filed under.
func (w *Writer) quarantine(ctx context.Context, row []any, cause error) (string, error) {
	document := map[string]any{}
	var key string
	for j, c := range w.schema.Columns {
		if w.sourceIdx[j] < 0 {
			continue
		}
		v := row[w.sourceIdx[j]]
		if b, ok := v.([]byte); ok {
			v = string(b)
		}
		document[c.Name] = v
		for _, k := range w.schema.Key {
			if k == c.Name {
				key = fmt.Sprintf("%v", v)
			}
		}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		encoded = []byte(fmt.Sprintf("%q", fmt.Sprint(document)))
	}
	_, err = w.db.pool.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s.%s (%s, table_name, row_key, error_message, row_data)
		 VALUES ($1, $2, $3, $4, $5)`, w.db.schema, QuarantineTable, InstanceColumn),
		w.instance, w.schema.Name, key, cause.Error(), encoded)
	return key, err
}
