// Package target writes to PostgreSQL.
package target

import (
	"fmt"
	"strings"
)

// PGType is a PostgreSQL column type as written in DDL.
type PGType string

const (
	Text        PGType = "text"
	BigInt      PGType = "bigint"
	DoublePrec  PGType = "double precision"
	Boolean     PGType = "boolean"
	TimestampTZ PGType = "timestamptz"
	JSONB       PGType = "jsonb"
	Bytea       PGType = "bytea"
	Numeric     PGType = "numeric"
)

var pgTypes = map[PGType]struct{}{
	Text: {}, BigInt: {}, DoublePrec: {}, Boolean: {},
	TimestampTZ: {}, JSONB: {}, Bytea: {}, Numeric: {},
}

// ParsePGType validates a type named in configuration.
func ParsePGType(s string) (PGType, error) {
	t := PGType(strings.ToLower(strings.TrimSpace(s)))
	if _, ok := pgTypes[t]; !ok {
		return "", fmt.Errorf("unsupported type %q; supported: %s", s, strings.Join(PGTypeNames(), ", "))
	}
	return t, nil
}

// PGTypeNames lists the supported target types, for error messages.
func PGTypeNames() []string {
	return []string{
		string(BigInt), string(Boolean), string(Bytea), string(DoublePrec),
		string(JSONB), string(Numeric), string(Text), string(TimestampTZ),
	}
}

// MapColumn returns the PostgreSQL type for a SQLite column.
//
// SQLite's declared types are advisory — it stores whatever it is given and
// applies only a loose affinity. Apiary in practice declares TEXT, INTEGER,
// REAL, BOOLEAN, TIMESTAMP and DATETIME, so those are mapped precisely and
// anything else falls back through SQLite's own affinity rules.
//
// A column named in json_columns becomes jsonb regardless of its declared type,
// because Apiary stores JSON documents in TEXT columns. That is never inferred:
// a TEXT column that merely happens to contain JSON stays text unless the
// catalog or the configuration says otherwise, so a stray non-JSON value cannot
// fail an insert.
func MapColumn(sqliteType string, isJSON bool) PGType {
	if isJSON {
		return JSONB
	}
	switch affinity(sqliteType) {
	case "BOOLEAN":
		return Boolean
	case "TIMESTAMP":
		return TimestampTZ
	case "INTEGER":
		return BigInt
	case "REAL":
		return DoublePrec
	case "BLOB":
		return Bytea
	case "NUMERIC":
		return Numeric
	default:
		return Text
	}
}

// affinity normalises a declared SQLite type to the bucket that decides the
// mapping. The rules follow SQLite's own determination order, with BOOLEAN and
// TIMESTAMP split out first because SQLite folds them into NUMERIC — which
// would lose exactly the distinction worth keeping in PostgreSQL.
func affinity(declared string) string {
	t := strings.ToUpper(strings.TrimSpace(declared))
	// pragma_table_info returns the declared type without constraints, but be
	// defensive: take the leading type token if anything else rides along.
	if i := strings.IndexAny(t, " ("); i > 0 {
		t = t[:i]
	}
	switch {
	case t == "":
		// No declared type at all: SQLite gives BLOB affinity.
		return "BLOB"
	case t == "BOOLEAN" || t == "BOOL":
		return "BOOLEAN"
	case strings.Contains(t, "TIMESTAMP") || t == "DATETIME" || t == "DATE":
		return "TIMESTAMP"
	case strings.Contains(t, "INT"):
		return "INTEGER"
	case strings.Contains(t, "CHAR"), strings.Contains(t, "CLOB"), strings.Contains(t, "TEXT"):
		return "TEXT"
	case strings.Contains(t, "BLOB"):
		return "BLOB"
	case strings.Contains(t, "REAL"), strings.Contains(t, "FLOA"), strings.Contains(t, "DOUB"):
		return "REAL"
	default:
		return "NUMERIC"
	}
}

// NeedsBoolConversion reports whether values read from SQLite must be coerced
// before being written to a PostgreSQL column of this type.
//
// SQLite has no boolean storage class: a column declared BOOLEAN holds 0 and 1.
// The driver hands those back as int64, which PostgreSQL will not accept into a
// boolean column, so the pipeline converts them on the way through.
func NeedsBoolConversion(t PGType) bool { return t == Boolean }
