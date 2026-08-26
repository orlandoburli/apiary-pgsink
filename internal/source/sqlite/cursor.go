package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/orlandoburli/apiary-pgsink/internal/catalog"
	"github.com/orlandoburli/apiary-pgsink/internal/config"
)

// CursorExpr is the SQL expression a cursor column is compared and recorded as.
//
// Integer cursors compare directly. Timestamp cursors must not: Apiary stores
// times as text with a local UTC offset, so a raw comparison is lexicographic
// and gets the order wrong the moment two rows carry different offsets.
//
//	'2026-08-07 02:00:00+00:00'  vs  '2026-08-06 23:00:00-04:00'
//
// The second is an hour later, and sorts first as text. A watermark taken that
// way skips rows on the next pass. strftime normalises to UTC with millisecond
// precision, which sorts correctly as text and keeps enough resolution that the
// overlap window is doing real work rather than papering over truncation.
func CursorExpr(kind catalog.CursorKind, column string) string {
	if kind == catalog.CursorTimestamp {
		return fmt.Sprintf("strftime('%%Y-%%m-%%d %%H:%%M:%%f', %s)", column)
	}
	return column
}

// MaxCursorValue reads the highest cursor position among the rows a table's
// filters actually select, normalised.
//
// Taken before a pass and recorded after it, so rows written during the pass
// stay above the watermark and are picked up next time rather than skipped.
//
// Applying the filters is not an optimisation, it is the correctness condition.
// An unfiltered maximum advances the watermark past rows the filter rejected —
// and a filter can reject a row temporarily. `status IN (success, failed)` on
// task_executions rejects a row while it is running; if the watermark had moved
// past its id in the meantime, the row would still be rejected on the pass that
// would have caught it settling, and would never arrive at all.
func MaxCursorValue(ctx context.Context, db *sql.DB, table string, c catalog.Cursor, filters []config.Filter) (string, error) {
	where, args := Compile(filters)
	query := fmt.Sprintf("SELECT MAX(%s) FROM %s", CursorExpr(c.Kind, c.Column), table)
	if where != "" {
		query += " WHERE " + where
	}
	var value sql.NullString
	if err := db.QueryRowContext(ctx, query, args...).Scan(&value); err != nil {
		return "", fmt.Errorf("read max %s.%s: %w", table, c.Column, err)
	}
	return value.String, nil
}

// Advance renders the predicate that selects rows at or after a watermark.
//
// inclusive widens `>` to `>=`, which mutable tables need: their cursor is
// updated_at, and two rows can share a millisecond. The idempotent upsert makes
// the resulting re-delivery free, and missing an update would not be.
//
// A row whose timestamp strftime cannot parse is excluded, and that is
// deliberate rather than a gap. Those are rows written before Apiary's
// _time_format fix, carrying Go's monotonic-clock suffix. Any row updated now
// gets a current-format timestamp, so an unparseable one is by definition not
// recently touched — the backfill already has it, and re-reading every legacy
// row on every pass would cost far more than it could ever find.
func Advance(c catalog.Cursor, watermark string, inclusive bool) (string, []any) {
	if watermark == "" {
		return "", nil
	}
	op := ">"
	if inclusive {
		op = ">="
	}
	return fmt.Sprintf("%s %s ?", CursorExpr(c.Kind, c.Column), op), []any{watermark}
}

// OpenRows renders the predicate that selects rows which have not settled.
//
// This is the whole point of the open_row class. task_executions and step_runs
// are inserted at dispatch and updated at completion, and neither carries
// updated_at — so a cursor alone would replicate the row with status='running'
// and zero cost, and never see the tokens, cost and timings that arrive later.
// Re-reading the unsettled rows every pass closes that gap, and the set is
// bounded by how many agents can run at once, so it is a handful of rows rather
// than a scan.
//
// A NULL or empty state counts as open: a row that has not reached a state yet
// is the last thing that should be treated as finished.
func OpenRows(s catalog.State) (string, []any) {
	if len(s.Terminal) == 0 {
		return "", nil
	}
	holders := ""
	args := make([]any, 0, len(s.Terminal))
	for i, v := range s.Terminal {
		if i > 0 {
			holders += ", "
		}
		holders += "?"
		args = append(args, v)
	}
	return fmt.Sprintf("(%s IS NULL OR %s NOT IN (%s))", s.Column, s.Column, holders), args
}
