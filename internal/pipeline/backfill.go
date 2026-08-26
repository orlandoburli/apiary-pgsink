// Package pipeline moves rows from an Apiary database into PostgreSQL.
package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"time"

	"github.com/orlandoburli/apiary-pgsink/internal/catalog"
	"github.com/orlandoburli/apiary-pgsink/internal/config"
	"github.com/orlandoburli/apiary-pgsink/internal/enrich"
	sqlitesrc "github.com/orlandoburli/apiary-pgsink/internal/source/sqlite"
	"github.com/orlandoburli/apiary-pgsink/internal/target"
)

// Runner carries everything a backfill or a sync pass needs.
type Runner struct {
	Source *sql.DB
	Live   catalog.LiveSchema
	Target *target.DB
	Plan   *config.Plan
	// Catalog resolves parent tables that the plan itself has disabled.
	Catalog *catalog.Catalog
	Out     io.Writer

	// instance is the source instance of the pass in flight, so the open-row
	// rescan can ask the target what it holds for this Apiary rather than any
	// other sharing the schema.
	instance string
	// Now is injected so runs are reproducible under test.
	Now func() time.Time
}

// TableResult reports what one table's pass did.
type TableResult struct {
	Table string
	Rows  int64
	// Quarantined counts rows the target refused, which were filed for an
	// operator rather than being allowed to stall the pass.
	Quarantined int64
	Skipped     bool
	Reason      string
	Elapsed     time.Duration
}

// Backfill loads history for every planned table.
//
// Each table's cursor high-water mark is read *before* its scan and recorded as
// the watermark afterwards. Rows written during the scan therefore sit above the
// watermark and are picked up by the first sync pass. Recording the mark after
// the scan instead would skip them: they would be below the new watermark but
// might never have been read.
func (r *Runner) Backfill(ctx context.Context, instance string) ([]TableResult, error) {
	var out []TableResult
	for _, planned := range r.Plan.Tables {
		result, err := r.backfillTable(ctx, instance, planned)
		if err != nil {
			return out, err
		}
		out = append(out, result)
	}
	return out, nil
}

func (r *Runner) backfillTable(ctx context.Context, instance string, planned config.Table) (TableResult, error) {
	started := r.now()
	result := TableResult{Table: planned.Name}

	live, ok := r.Live[planned.Name]
	if !ok {
		result.Skipped, result.Reason = true, "not in this Apiary database"
		return result, nil
	}
	columns := planned.Columns(live)
	if len(columns) == 0 {
		result.Skipped, result.Reason = true, "every column is excluded"
		return result, nil
	}

	schema, err := target.Desired(planned, live)
	if err != nil {
		return result, err
	}
	writer := target.NewWriter(r.Target, schema, instance, columns)

	// Read the cursor position before scanning, so concurrent writes land above
	// the watermark rather than being skipped.
	var mark target.Watermark
	mark.Table = planned.Name
	if c := planned.Catalog.Cursor; c != nil {
		mark.Kind = string(c.Kind)
		if mark.Value, err = sqlitesrc.MaxCursorValue(ctx, r.Source, planned.Name, *c, planned.Filters); err != nil {
			return result, err
		}
	} else {
		mark.Kind = string(planned.Catalog.Class)
	}

	fields, err := enrich.Prepare(planned.ExtraFields, enrich.Context{
		Instance: instance, Table: planned.Name, Now: started,
	})
	if err != nil {
		return result, err
	}

	var afterRowID int64
	for {
		page, err := sqlitesrc.Batch(ctx, r.Source, planned.Name, columns, planned.Filters, afterRowID, r.Plan.Sync.BatchSize)
		if err != nil {
			return result, err
		}
		if len(page.Rows) == 0 {
			break
		}
		extras, err := buildExtras(fields, enrich.Context{Instance: instance, Table: planned.Name, Now: started}, columns, page.Rows)
		if err != nil {
			return result, err
		}
		_, bad, err := writer.WriteBatchIsolating(ctx, page.Rows, extras)
		if err != nil {
			return result, err
		}
		result.Quarantined += int64(len(bad))
		result.Rows += int64(len(page.Rows))
		afterRowID = page.LastRowID

		// The watermark advances only after a batch is committed, so an
		// interrupted backfill resumes from data that is actually in the
		// target rather than from data that was merely read.
		if err := r.Target.SetWatermark(ctx, instance, mark, int64(len(page.Rows))); err != nil {
			return result, err
		}
		if len(page.Rows) < r.Plan.Sync.BatchSize {
			break
		}
	}

	result.Elapsed = r.now().Sub(started)
	return result, nil
}

func buildExtras(fields []enrich.Field, ctx enrich.Context, columns []string, rows [][]any) ([][]target.ExtraValue, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	out := make([][]target.ExtraValue, len(rows))
	for i, row := range rows {
		values := make([]target.ExtraValue, 0, len(fields))
		for _, f := range fields {
			v, err := f.Value(ctx, columns, row)
			if err != nil {
				return nil, fmt.Errorf("table %s: %w", ctx.Table, err)
			}
			values = append(values, target.ExtraValue{Name: f.Name, Value: v})
		}
		out[i] = values
	}
	return out, nil
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
