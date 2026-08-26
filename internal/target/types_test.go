package target

import "testing"

func TestMapColumnCoversApiarysDeclaredTypes(t *testing.T) {
	// Every declared type that appears in Apiary's schema, and what it must
	// become. TIMESTAMP and BOOLEAN are the interesting ones: SQLite folds both
	// into NUMERIC affinity, which would throw away the distinction.
	cases := []struct {
		sqlite string
		want   PGType
	}{
		{"TEXT", Text},
		{"INTEGER", BigInt},
		{"REAL", DoublePrec},
		{"BOOLEAN", Boolean},
		{"TIMESTAMP", TimestampTZ},
		{"DATETIME", TimestampTZ},
	}
	for _, c := range cases {
		if got := MapColumn(c.sqlite, false); got != c.want {
			t.Errorf("MapColumn(%q) = %q, want %q", c.sqlite, got, c.want)
		}
	}
}

func TestMapColumnIsDefensiveAboutDeclarations(t *testing.T) {
	cases := []struct {
		sqlite string
		want   PGType
	}{
		{"text", Text},
		{"  Integer  ", BigInt},
		{"VARCHAR(255)", Text},
		{"BIGINT", BigInt},
		{"DOUBLE PRECISION", DoublePrec},
		{"BLOB", Bytea},
		{"DECIMAL(10,2)", Numeric},
		{"", Bytea}, // no declared type is BLOB affinity in SQLite
		{"INTEGER PRIMARY KEY AUTOINCREMENT", BigInt},
	}
	for _, c := range cases {
		if got := MapColumn(c.sqlite, false); got != c.want {
			t.Errorf("MapColumn(%q) = %q, want %q", c.sqlite, got, c.want)
		}
	}
}

// JSON is declared, never sniffed. Apiary keeps JSON documents in TEXT columns,
// but a TEXT column that merely looks like JSON must stay text — otherwise one
// stray value fails the insert for the whole batch.
func TestJSONIsDeclaredNotInferred(t *testing.T) {
	if got := MapColumn("TEXT", true); got != JSONB {
		t.Errorf("declared json column = %q, want jsonb", got)
	}
	if got := MapColumn("TEXT", false); got != Text {
		t.Errorf("undeclared column = %q, want text", got)
	}
}

func TestBooleanNeedsConversion(t *testing.T) {
	// SQLite has no boolean storage class; a BOOLEAN column holds 0/1 and the
	// driver returns int64, which PostgreSQL will not take.
	if !NeedsBoolConversion(MapColumn("BOOLEAN", false)) {
		t.Error("boolean columns must be converted on the way through")
	}
	if NeedsBoolConversion(MapColumn("INTEGER", false)) {
		t.Error("a plain integer column needs no conversion")
	}
}

func TestParsePGType(t *testing.T) {
	if _, err := ParsePGType("TimestampTZ"); err != nil {
		t.Errorf("case-insensitive parse failed: %v", err)
	}
	if _, err := ParsePGType("money"); err == nil {
		t.Error("unsupported type must be rejected")
	}
}
