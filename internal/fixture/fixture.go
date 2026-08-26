// Package fixture builds throwaway Apiary databases from the schema snapshots
// in testdata/schema.
//
// The snapshots are captured from real apiary release binaries by
// scripts/capture-schema.sh, so the test suite is pinned to what Apiary
// actually ships rather than to a hand-maintained copy of its DDL.
package fixture

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// Latest is the snapshot used by tests that just need "a current Apiary".
const Latest = "0.17"

// schemaDir locates testdata/schema relative to this source file, so tests work
// from any package without threading relative paths around.
func schemaDir() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "testdata", "schema")
}

// Labels lists every captured snapshot, oldest first by string order.
func Labels() ([]string, error) {
	entries, err := os.ReadDir(schemaDir())
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "apiary-") && strings.HasSuffix(name, ".sql") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(name, "apiary-"), ".sql"))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Build materialises the named snapshot into a temporary database and returns
// an open handle. The file is removed when the test ends.
func Build(t *testing.T, label string) *sql.DB {
	t.Helper()
	ddl, err := os.ReadFile(filepath.Join(schemaDir(), fmt.Sprintf("apiary-%s.sql", label)))
	if err != nil {
		t.Fatalf("read schema snapshot %s: %v", label, err)
	}
	path := filepath.Join(t.TempDir(), "apiary.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("apply schema snapshot %s: %v", label, err)
	}
	return db
}

// Path is Build for callers that need a file path rather than a handle — the
// read-only open path in the sqlite package, for instance.
func Path(t *testing.T, label string) string {
	t.Helper()
	ddl, err := os.ReadFile(filepath.Join(schemaDir(), fmt.Sprintf("apiary-%s.sql", label)))
	if err != nil {
		t.Fatalf("read schema snapshot %s: %v", label, err)
	}
	path := filepath.Join(t.TempDir(), "apiary.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		db.Close()
		t.Fatalf("apply schema snapshot %s: %v", label, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}
	return path
}
