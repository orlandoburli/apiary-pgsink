package pipeline_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/orlandoburli/apiary-pgsink/internal/catalog"
	"github.com/orlandoburli/apiary-pgsink/internal/config"
	"github.com/orlandoburli/apiary-pgsink/internal/fixture"
	sqlitesrc "github.com/orlandoburli/apiary-pgsink/internal/source/sqlite"
	"github.com/orlandoburli/apiary-pgsink/internal/target"
)

// DSNEnv names the environment variable that points these tests at a
// PostgreSQL. Without it they skip, so `go test ./...` still works on a machine
// with no Docker — `make pg-up` starts a throwaway container and prints it.
const DSNEnv = "PGSINK_TEST_DSN"

// newTarget connects to the test PostgreSQL and hands back a schema of its own,
// dropped when the test finishes. Each test therefore gets a clean namespace
// and they can run in parallel against one container.
func newTarget(t *testing.T) (*target.DB, string) {
	t.Helper()
	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		t.Skipf("set %s to run this (make pg-up prints one)", DSNEnv)
	}
	schema := "t_" + strings.ToLower(strings.NewReplacer("/", "", "-", "_", " ", "_").Replace(t.Name()))
	if len(schema) > 60 {
		schema = schema[:60]
	}
	ctx := context.Background()

	admin, err := target.Open(ctx, dsn, schema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Pool().Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema)); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := admin.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Pool().Exec(context.Background(), fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema))
		admin.Close()
	})
	return admin, schema
}

// newSource builds a fixture Apiary database and returns it plus its reflected
// schema.
func newSource(t *testing.T, seed func(*sql.DB)) (*sql.DB, catalog.LiveSchema) {
	t.Helper()
	db := fixture.Build(t, fixture.Latest)
	if seed != nil {
		seed(db)
	}
	live, err := sqlitesrc.Reflect(context.Background(), db)
	if err != nil {
		t.Fatalf("reflect source: %v", err)
	}
	return db, live
}

// newPlan resolves a configuration body against the catalog.
func newPlan(t *testing.T, body string) *config.Plan {
	t.Helper()
	f, err := config.Parse([]byte(`
source: {dsn: fixture.db, instance: test}
target: {dsn: "postgres://ignored/ignored"}
` + body))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cat, err := catalog.Load()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	plan, errs := config.Resolve(f, cat)
	if len(errs) > 0 {
		t.Fatalf("resolve: %v", errs)
	}
	return plan
}

// only narrows a plan to the named tables, so a test exercises one table rather
// than all twenty-four.
func only(plan *config.Plan, names ...string) *config.Plan {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var kept []config.Table
	for _, tbl := range plan.Tables {
		if want[tbl.Name] {
			kept = append(kept, tbl)
		}
	}
	plan.Tables = kept
	return plan
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("seed: %v\n%s", err, query)
	}
}

func scalar[T any](t *testing.T, db *target.DB, query string, args ...any) T {
	t.Helper()
	var v T
	if err := db.Pool().QueryRow(context.Background(), query, args...).Scan(&v); err != nil {
		t.Fatalf("query: %v\n%s", err, query)
	}
	return v
}

// migrateAll applies the DDL a plan needs, the way `pgsink migrate` does.
func migrateAll(t *testing.T, db *target.DB, plan *config.Plan, live catalog.LiveSchema) {
	t.Helper()
	ctx := context.Background()
	for _, planned := range plan.Tables {
		lt, ok := live[planned.Name]
		if !ok {
			continue
		}
		want, err := target.Desired(planned, lt)
		if err != nil {
			t.Fatalf("desired %s: %v", planned.Name, err)
		}
		have, err := db.Reflect(ctx, planned.Name)
		if err != nil {
			t.Fatalf("reflect %s: %v", planned.Name, err)
		}
		if err := db.Apply(ctx, target.Diff(db.Schema(), want, have)); err != nil {
			t.Fatalf("apply %s: %v", planned.Name, err)
		}
	}
}
