package target

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orlandoburli/apiary-pgsink/internal/pgtype"
)

// DB is a connection pool to the target, scoped to one schema.
type DB struct {
	pool   *pgxpool.Pool
	schema string
}

// Open connects to the target and verifies it is reachable.
func Open(ctx context.Context, dsn, schema string) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to target: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping target: %w", err)
	}
	return &DB{pool: pool, schema: schema}, nil
}

// Close releases the pool.
func (d *DB) Close() { d.pool.Close() }

// Schema is the target schema name.
func (d *DB) Schema() string { return d.schema }

// Pool exposes the underlying pool for the pipeline's batched writes.
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

// EnsureSchema creates the target schema and the watermark table if they are
// absent. Both are idempotent, so migrate can be re-run safely.
func (d *DB) EnsureSchema(ctx context.Context) error {
	if _, err := d.pool.Exec(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", d.schema)); err != nil {
		return fmt.Errorf("create schema %s: %w", d.schema, err)
	}
	if _, err := d.pool.Exec(ctx, StateTableSQL(d.schema)); err != nil {
		return fmt.Errorf("create %s.%s: %w", d.schema, StateTable, err)
	}
	return nil
}

// Reflect reads the columns of a target table, keyed by name. An empty map
// means the table does not exist yet.
func (d *DB) Reflect(ctx context.Context, table string) (map[string]pgtype.PGType, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT column_name, data_type, udt_name
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2`, d.schema, table)
	if err != nil {
		return nil, fmt.Errorf("reflect %s.%s: %w", d.schema, table, err)
	}
	defer rows.Close()
	out := map[string]pgtype.PGType{}
	for rows.Next() {
		var name, dataType, udt string
		if err := rows.Scan(&name, &dataType, &udt); err != nil {
			return nil, err
		}
		out[name] = normalizePGType(dataType, udt)
	}
	return out, rows.Err()
}

// normalizePGType folds information_schema's spelling back into the vocabulary
// used to generate the DDL, so a diff compares like with like. Postgres reports
// "timestamp with time zone" for what was written as timestamptz, and
// "USER-DEFINED"/"ARRAY" with the real name only in udt_name.
func normalizePGType(dataType, udt string) pgtype.PGType {
	switch dataType {
	case "timestamp with time zone":
		return pgtype.TimestampTZ
	case "character varying", "text":
		return pgtype.Text
	case "bigint":
		return pgtype.BigInt
	case "integer", "smallint":
		// Narrower than what pgsink generates, but not a data risk: report it
		// as bigint so an existing hand-made column does not read as a
		// conflict on every run.
		return pgtype.BigInt
	case "double precision", "real":
		return pgtype.DoublePrec
	case "boolean":
		return pgtype.Boolean
	case "jsonb":
		return pgtype.JSONB
	case "bytea":
		return pgtype.Bytea
	case "numeric":
		return pgtype.Numeric
	default:
		if udt == "jsonb" {
			return pgtype.JSONB
		}
		return pgtype.Text
	}
}

// Apply runs a set of changes in one transaction, so a failed migrate leaves
// the target exactly as it was.
func (d *DB) Apply(ctx context.Context, changes []Change) error {
	stmts := make([]string, 0, len(changes))
	for _, c := range changes {
		if c.Blocking {
			return fmt.Errorf("table %s: %s", c.Table, c.Reason)
		}
		stmts = append(stmts, c.SQL)
	}
	if len(stmts) == 0 {
		return nil
	}
	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		for _, sql := range stmts {
			if _, err := tx.Exec(ctx, sql); err != nil {
				return fmt.Errorf("%s: %w", firstLine(sql), err)
			}
		}
		return nil
	})
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}
