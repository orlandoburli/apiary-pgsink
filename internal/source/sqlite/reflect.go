// Package sqlite reads an Apiary SQLite database. It never writes.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver, matching Apiary's CGO-free build

	"github.com/orlandoburli/apiary-pgsink/internal/catalog"
)

// Open opens an Apiary database for reading.
//
// A note on "read-only": SQLite in WAL mode has no true read-only reader. The
// process must be able to write the -shm wal-index file, or create it in the
// containing directory. Running as a user with only read bits fails here with
// "unable to open database file", which reads like a path bug and is not one —
// run pgsink as the daemon's user, or share a group with write access to the
// data directory.
//
// immutable=1 would silence that error and is never the answer: it promises
// SQLite the file cannot change while the daemon is actively writing to it, so
// reads go quietly wrong instead of failing loudly.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", path, err)
	}
	db, err := sql.Open("sqlite", readOnlyDSN(abs))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", abs, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		if strings.Contains(err.Error(), "unable to open database file") {
			return nil, fmt.Errorf("open %s: %w\n"+
				"  either the file does not exist, or this process cannot write the WAL index.\n"+
				"  SQLite in WAL mode needs write access to the -shm file and the containing\n"+
				"  directory even for readers — run pgsink as the Apiary daemon's user, or\n"+
				"  share a group with write access to the data directory.", abs, err)
		}
		return nil, fmt.Errorf("ping %s: %w", abs, err)
	}
	return db, nil
}

// readOnlyDSN builds a SQLite URI that opens for reading and refuses to create
// the file.
//
// The `file:` prefix is load-bearing. modernc.org/sqlite silently ignores query
// parameters it does not recognise on a bare path, so a plain
// "/path/apiary.db?mode=ro" is not read-only at all — and worse, a mistyped
// path creates an empty database rather than failing, after which pgsink would
// cheerfully replicate nothing. Only the URI form reaches SQLite's own
// parameter handling, where mode=ro is honoured and a missing file is an error.
func readOnlyDSN(abs string) string {
	// SQLite URI filenames percent-decode, so encode the path but keep the
	// separators intact.
	escaped := strings.ReplaceAll(url.PathEscape(abs), "%2F", "/")
	return "file:" + escaped + "?" + url.Values{
		"mode":    {"ro"},
		"_pragma": {"busy_timeout(5000)", "query_only(true)"},
	}.Encode()
}

// Reflect reads the live table shapes. This is what makes an Apiary upgrade
// that adds columns a non-event: pgsink replicates what it finds rather than
// what it was compiled against.
func Reflect(ctx context.Context, db *sql.DB) (catalog.LiveSchema, error) {
	names, err := tableNames(ctx, db)
	if err != nil {
		return nil, err
	}
	schema := make(catalog.LiveSchema, len(names))
	for _, name := range names {
		table, err := reflectTable(ctx, db, name)
		if err != nil {
			return nil, err
		}
		schema[name] = table
	}
	return schema, nil
}

func tableNames(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func reflectTable(ctx context.Context, db *sql.DB, name string) (catalog.LiveTable, error) {
	table := catalog.LiveTable{Name: name}
	// pragma_table_info is a table-valued function, so the table name binds as
	// an ordinary parameter — no string interpolation, no injection surface.
	rows, err := db.QueryContext(ctx,
		`SELECT name, type, pk FROM pragma_table_info(?) ORDER BY cid`, name)
	if err != nil {
		return table, fmt.Errorf("reflect %s: %w", name, err)
	}
	defer rows.Close()
	type pkCol struct {
		name string
		pos  int
	}
	var pks []pkCol
	for rows.Next() {
		var col catalog.LiveColumn
		var pk int
		if err := rows.Scan(&col.Name, &col.Type, &pk); err != nil {
			return table, fmt.Errorf("reflect %s: %w", name, err)
		}
		table.Columns = append(table.Columns, col)
		if pk > 0 {
			pks = append(pks, pkCol{col.Name, pk})
		}
	}
	if err := rows.Err(); err != nil {
		return table, fmt.Errorf("reflect %s: %w", name, err)
	}
	// pk is the 1-based position within a composite key, not a boolean.
	for i := 1; i <= len(pks); i++ {
		for _, c := range pks {
			if c.pos == i {
				table.PrimaryKey = append(table.PrimaryKey, c.name)
			}
		}
	}
	return table, nil
}
