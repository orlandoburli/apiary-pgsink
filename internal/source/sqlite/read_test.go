package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/orlandoburli/apiary-pgsink/internal/config"
)

func compiled(t *testing.T, filters ...config.Filter) (string, []any) {
	t.Helper()
	return Compile(filters)
}

// The negative operators must admit NULLs, or "everything except failures"
// silently loses every row that has no status at all.
func TestNegativeOperatorsAdmitNulls(t *testing.T) {
	for _, f := range []config.Filter{
		{Column: "status", Op: config.OpNe, Value: "failed"},
		{Column: "status", Op: config.OpNotIn, Value: []any{"failed", "cancelled"}},
	} {
		where, _ := compiled(t, f)
		if !strings.Contains(where, "status IS NULL") {
			t.Errorf("%s compiled to %q, which drops rows with no value", f.Op, where)
		}
	}
}

// The positive ones must not: a NULL is not equal to, or greater than, anything.
func TestPositiveOperatorsDoNotAdmitNulls(t *testing.T) {
	for _, f := range []config.Filter{
		{Column: "status", Op: config.OpEq, Value: "success"},
		{Column: "cost_usd", Op: config.OpGt, Value: 0},
		{Column: "status", Op: config.OpIn, Value: []any{"a", "b"}},
		{Column: "title", Op: config.OpLike, Value: "%x%"},
	} {
		where, _ := compiled(t, f)
		if strings.Contains(where, "IS NULL") {
			t.Errorf("%s compiled to %q, which should not admit nulls", f.Op, where)
		}
	}
}

// Verified against SQLite rather than asserted about the SQL text: the point is
// the rows that come back.
func TestFilterSemanticsAgainstSQLite(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, status TEXT, cost REAL);
		INSERT INTO t (id, status, cost) VALUES
		  (1,'success',1.0), (2,'failed',2.0), (3,NULL,3.0), (4,'running',NULL)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cases := []struct {
		name   string
		filter config.Filter
		want   []int64
	}{
		{"eq", config.Filter{Column: "status", Op: config.OpEq, Value: "success"}, []int64{1}},
		{"ne admits null", config.Filter{Column: "status", Op: config.OpNe, Value: "failed"}, []int64{1, 3, 4}},
		{"in", config.Filter{Column: "status", Op: config.OpIn, Value: []any{"success", "failed"}}, []int64{1, 2}},
		{"not_in admits null", config.Filter{Column: "status", Op: config.OpNotIn, Value: []any{"failed"}}, []int64{1, 3, 4}},
		{"gt", config.Filter{Column: "cost", Op: config.OpGt, Value: 1.5}, []int64{2, 3}},
		{"is_null", config.Filter{Column: "status", Op: config.OpIsNull}, []int64{3}},
		{"not_null", config.Filter{Column: "cost", Op: config.OpNotNull}, []int64{1, 2, 3}},
		{"like", config.Filter{Column: "status", Op: config.OpLike, Value: "s%"}, []int64{1}},
		{"or_null widens", config.Filter{Column: "cost", Op: config.OpGte, Value: 2.0, OrNull: true}, []int64{2, 3, 4}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			where, args := compiled(t, c.filter)
			query := "SELECT id FROM t"
			if where != "" {
				query += " WHERE " + where
			}
			rows, err := db.Query(query+" ORDER BY id", args...)
			if err != nil {
				t.Fatalf("%s: %v\n%s", err, query, where)
			}
			defer rows.Close()
			var got []int64
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				got = append(got, id)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v (%s)", got, c.want, where)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v (%s)", got, c.want, where)
				}
			}
		})
	}
}

// Filter values come from a configuration file. They are always bound as
// parameters, never interpolated, even though the column name beside them is
// validated as an identifier.
func TestFilterValuesAreAlwaysBound(t *testing.T) {
	where, args := compiled(t, config.Filter{
		Column: "status", Op: config.OpEq, Value: "'; DROP TABLE tasks; --",
	})
	if strings.Contains(where, "DROP") {
		t.Fatalf("the value was interpolated into the SQL: %q", where)
	}
	if len(args) != 1 || args[0] != "'; DROP TABLE tasks; --" {
		t.Errorf("args = %v, want the value bound", args)
	}
}

func TestMultipleFiltersAreAnded(t *testing.T) {
	where, args := compiled(t,
		config.Filter{Column: "status", Op: config.OpEq, Value: "success"},
		config.Filter{Column: "cost", Op: config.OpGt, Value: 1},
	)
	if !strings.Contains(where, " AND ") {
		t.Errorf("where = %q, want the filters combined with AND", where)
	}
	if len(args) != 2 {
		t.Errorf("args = %v, want two", args)
	}
}

func TestEmptyFilterSetCompilesToNothing(t *testing.T) {
	if where, args := compiled(t); where != "" || len(args) != 0 {
		t.Errorf("Compile(nil) = %q, %v; want empty", where, args)
	}
}

func TestBatchRejectsAnEmptyProjection(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := Batch(context.Background(), db, "t", nil, nil, 0, 10); err == nil {
		t.Fatal("reading no columns should be an error, not an empty result")
	}
}
