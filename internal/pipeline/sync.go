package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/orlandoburli/apiary-pgsink/internal/catalog"
	"github.com/orlandoburli/apiary-pgsink/internal/config"
	"github.com/orlandoburli/apiary-pgsink/internal/enrich"
	sqlitesrc "github.com/orlandoburli/apiary-pgsink/internal/source/sqlite"
	"github.com/orlandoburli/apiary-pgsink/internal/target"
)

// Pass is one sweep over every table.
type Pass struct {
	Results []TableResult
	Rows    int64
	Elapsed time.Duration
}

// Sync runs one incremental pass: for each table, read whatever has changed
// since its watermark and upsert it.
//
// What "changed" means is per class, because Apiary writes its tables five
// different ways. See the catalog for the reasoning; this is where it is
// applied.
func (r *Runner) Sync(ctx context.Context, instance string) (*Pass, error) {
	started := r.now()
	marks, err := r.Target.Watermarks(ctx, instance)
	if err != nil {
		return nil, err
	}
	pass := &Pass{}
	for _, planned := range r.Plan.Tables {
		if err := ctx.Err(); err != nil {
			return pass, err
		}
		result, err := r.syncTable(ctx, instance, planned, marks[planned.Name])
		if err != nil {
			return pass, err
		}
		pass.Results = append(pass.Results, result)
		pass.Rows += result.Rows
	}
	pass.Elapsed = r.now().Sub(started)
	return pass, nil
}

func (r *Runner) syncTable(ctx context.Context, instance string, planned config.Table, mark target.Watermark) (TableResult, error) {
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

	overlap, err := r.Plan.OverlapDuration()
	if err != nil {
		return result, err
	}

	// Read the next watermark before selecting rows, so anything written during
	// the pass stays above it.
	next := target.Watermark{Table: planned.Name, Kind: mark.Kind, Value: mark.Value}
	if c := planned.Catalog.Cursor; c != nil {
		next.Kind = string(c.Kind)
		if next.Value, err = sqlitesrc.MaxCursorValue(ctx, r.Source, planned.Name, *c, planned.Filters); err != nil {
			return result, err
		}
	} else {
		next.Kind = string(planned.Catalog.Class)
	}

	r.instance = instance
	selectors, err := r.selectors(ctx, planned, mark, overlap)
	if err != nil {
		return result, err
	}

	fields, err := enrich.Prepare(planned.ExtraFields, enrich.Context{
		Instance: instance, Table: planned.Name, Now: started,
	})
	if err != nil {
		return result, err
	}
	ectx := enrich.Context{Instance: instance, Table: planned.Name, Now: started}

	for _, sel := range selectors {
		var afterRowID int64
		for {
			page, err := sqlitesrc.Batch(ctx, r.Source, planned.Name, columns,
				planned.Filters, afterRowID, r.Plan.Sync.BatchSize, sel)
			if err != nil {
				return result, err
			}
			if len(page.Rows) == 0 {
				break
			}
			extras, err := buildExtras(fields, ectx, columns, page.Rows)
			if err != nil {
				return result, err
			}
			if _, err := writer.WriteBatch(ctx, page.Rows, extras); err != nil {
				return result, err
			}
			result.Rows += int64(len(page.Rows))
			afterRowID = page.LastRowID
			if len(page.Rows) < r.Plan.Sync.BatchSize {
				break
			}
		}
	}

	// The watermark advances only once every selector has been drained and
	// committed, so an interrupted pass repeats work rather than skipping it.
	if err := r.Target.SetWatermark(ctx, instance, next, result.Rows); err != nil {
		return result, err
	}
	result.Elapsed = r.now().Sub(started)
	return result, nil
}

// selectors returns the extra predicates that define "changed" for this table.
// More than one means the same row may be read twice in a pass; the upsert makes
// that free.
// rescanOpenLimit caps how many unsettled rows one pass re-reads. Apiary cannot
// have more than a few dozen genuinely in flight; a larger number means a target
// left full of rows interrupted by a crash, and those converge over several
// passes rather than stalling one.
const rescanOpenLimit = 5000

// rescanChunk keeps the generated IN list to a size SQLite is comfortable with —
// its default parameter limit is 999.
const rescanChunk = 500

// rescanOpen builds the predicates that re-read rows the target still holds as
// unsettled.
func (r *Runner) rescanOpen(ctx context.Context, planned config.Table) ([]sqlitesrc.Selector, error) {
	ct := planned.Catalog
	if ct.State == nil || len(ct.Key) != 1 {
		return nil, nil
	}
	keys, err := r.Target.OpenKeys(ctx, r.instance, planned.Name, ct.Key[0], ct.State.Column, ct.State.Terminal, rescanOpenLimit)
	if err != nil {
		return nil, err
	}
	var out []sqlitesrc.Selector
	for start := 0; start < len(keys); start += rescanChunk {
		end := start + rescanChunk
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		holders := ""
		for i := range chunk {
			if i > 0 {
				holders += ", "
			}
			holders += "?"
		}
		out = append(out, sqlitesrc.Selector{
			SQL:  fmt.Sprintf("%s IN (%s)", ct.Key[0], holders),
			Args: append([]any(nil), chunk...),
		})
	}
	return out, nil
}

func (r *Runner) selectors(ctx context.Context, planned config.Table, mark target.Watermark, overlap time.Duration) ([]sqlitesrc.Selector, error) {
	ct := planned.Catalog
	switch ct.Class {
	case catalog.ClassAppendOnly:
		_ = ctx
		// Insert-only: everything past the watermark, and nothing before it.
		return []sqlitesrc.Selector{advance(ct, mark.Value, false, overlap)}, nil

	case catalog.ClassMutable:
		// updated_at moves when the row does, so the watermark is inclusive and
		// stepped back by the overlap window: two rows can share a millisecond,
		// and clocks are adjusted.
		return []sqlitesrc.Selector{advance(ct, back(mark.Value, ct, overlap), true, overlap)}, nil

	case catalog.ClassOpenRow:
		// Two selectors, and both are necessary.
		//
		// The cursor finds rows inserted since the last pass. The rescan
		// re-reads the rows the *target* still holds as unsettled — not the
		// ones the source reports as unsettled now. A row that completed
		// between passes is settled in the source and its cursor has not moved,
		// so a source-side "what is running?" misses exactly the completion
		// this class exists to capture.
		out := []sqlitesrc.Selector{advance(ct, mark.Value, false, overlap)}
		rescan, err := r.rescanOpen(ctx, planned)
		if err != nil {
			return nil, err
		}
		return append(out, rescan...), nil

	case catalog.ClassFollowParent:
		// No cursor of its own: read the children of every parent that moved.
		return r.followParent(ctx, planned, mark, overlap)

	case catalog.ClassSnapshot:
		// Small enough to compare wholesale.
		return []sqlitesrc.Selector{{}}, nil

	default:
		return nil, fmt.Errorf("table %s: unknown class %q", planned.Name, ct.Class)
	}
}

// followParent builds a predicate selecting rows whose parent has moved since
// the watermark, using the parent's own cursor.
func (r *Runner) followParent(ctx context.Context, planned config.Table, mark target.Watermark, overlap time.Duration) ([]sqlitesrc.Selector, error) {
	p := planned.Catalog.Parent
	parent, ok := r.parentEntry(p.Table)
	if !ok {
		return nil, fmt.Errorf("table %s: parent %q is not in the catalog", planned.Name, p.Table)
	}
	if _, ok := r.Live[p.Table]; !ok {
		// The parent is absent from this Apiary version, so the child cannot be
		// followed either. Replicating it wholesale is the safe reading.
		return []sqlitesrc.Selector{{}}, nil
	}
	if parent.Cursor == nil || mark.Value == "" {
		return []sqlitesrc.Selector{{}}, nil
	}
	expr, args := sqlitesrc.Advance(*parent.Cursor, back(mark.Value, parent, overlap), true)
	if expr == "" {
		return []sqlitesrc.Selector{{}}, nil
	}
	sub := fmt.Sprintf("%s IN (SELECT %s FROM %s WHERE %s)", p.Local, p.Remote, p.Table, expr)
	return []sqlitesrc.Selector{{SQL: sub, Args: args}}, nil
}

func (r *Runner) parentEntry(name string) (*catalog.Table, bool) {
	for i := range r.Plan.Tables {
		if r.Plan.Tables[i].Name == name {
			return r.Plan.Tables[i].Catalog, true
		}
	}
	if r.Catalog != nil {
		return r.Catalog.Table(name)
	}
	return nil, false
}

func advance(ct *catalog.Table, watermark string, inclusive bool, _ time.Duration) sqlitesrc.Selector {
	if ct.Cursor == nil || watermark == "" {
		return sqlitesrc.Selector{}
	}
	expr, args := sqlitesrc.Advance(*ct.Cursor, watermark, inclusive)
	return sqlitesrc.Selector{SQL: expr, Args: args}
}

// back steps a timestamp watermark into the past by the overlap window.
// Integer cursors are exact and need no window.
func back(watermark string, ct *catalog.Table, overlap time.Duration) string {
	if watermark == "" || ct.Cursor == nil || ct.Cursor.Kind != catalog.CursorTimestamp {
		return watermark
	}
	t, err := time.Parse(sqlitesrc.WatermarkLayout, watermark)
	if err != nil {
		// Not a shape we wrote; leave it alone rather than guessing and
		// silently widening or narrowing the window.
		return watermark
	}
	return t.Add(-overlap).Format(sqlitesrc.WatermarkLayout)
}
