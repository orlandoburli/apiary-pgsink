// Package catalog describes how pgsink follows each Apiary table.
//
// The catalog is data, not code: catalog/tables.yaml is the source of truth and
// is embedded at build time. Code here only parses it, validates it, and
// compares it against a live Apiary schema.
package catalog

import (
	"fmt"
	"sort"
	"strings"

	_ "embed"

	"gopkg.in/yaml.v3"
)

//go:embed tables.yaml
var embedded []byte

// Class determines how a table is followed. See tables.yaml for the prose.
type Class string

const (
	// ClassAppendOnly covers tables whose rows are never updated after insert.
	ClassAppendOnly Class = "append_only"
	// ClassMutable covers tables updated in place that carry updated_at.
	ClassMutable Class = "mutable"
	// ClassOpenRow covers tables updated in place with no updated_at, where
	// rows in a non-terminal state are re-read every cycle.
	ClassOpenRow Class = "open_row"
	// ClassFollowParent covers tables with no usable cursor of their own.
	ClassFollowParent Class = "follow_parent"
	// ClassSnapshot covers tables small enough to compare wholesale.
	ClassSnapshot Class = "snapshot"
)

var classes = map[Class]struct{}{
	ClassAppendOnly: {}, ClassMutable: {}, ClassOpenRow: {},
	ClassFollowParent: {}, ClassSnapshot: {},
}

// CursorKind is how a cursor column is compared and serialised.
type CursorKind string

const (
	CursorInteger   CursorKind = "integer"
	CursorTimestamp CursorKind = "timestamp"
)

// Cursor is the column a table advances on.
type Cursor struct {
	Column string     `yaml:"column"`
	Kind   CursorKind `yaml:"kind"`
}

// State names the column that says whether a row has settled, and which values
// mean it has. Only open_row requires it; other classes may declare it for
// documentation and for filter validation.
type State struct {
	Column   string   `yaml:"column"`
	Terminal []string `yaml:"terminal"`
}

// IsTerminal reports whether value is one of the settled states. A NULL or
// empty state is never terminal: a row that has not reached a state yet is
// still open, and must keep being re-read.
func (s State) IsTerminal(value string) bool {
	for _, t := range s.Terminal {
		if t == value {
			return true
		}
	}
	return false
}

// Parent links a follow_parent table to the table whose movement drives it.
type Parent struct {
	Table  string `yaml:"table"`
	Local  string `yaml:"local"`
	Remote string `yaml:"remote"`
}

// Table is one entry in the catalog.
type Table struct {
	Name   string   `yaml:"name"`
	Class  Class    `yaml:"class"`
	Key    []string `yaml:"key"`
	Cursor *Cursor  `yaml:"cursor,omitempty"`
	State  *State   `yaml:"state,omitempty"`
	Parent *Parent  `yaml:"parent,omitempty"`
	// TimeColumn is this table's own time dimension, which a `since` window
	// resolves against. Empty for tables that have none.
	TimeColumn   string   `yaml:"time_column,omitempty"`
	Timestamps   []string `yaml:"timestamps,omitempty"`
	JSONColumns  []string `yaml:"json_columns,omitempty"`
	LargeColumns []string `yaml:"large_columns,omitempty"`
	Volume       string   `yaml:"volume,omitempty"`
	Notes        string   `yaml:"notes,omitempty"`
}

// Catalog is the whole file.
type Catalog struct {
	SchemaVersion int     `yaml:"schema_version"`
	ApiaryCompat  string  `yaml:"apiary_compat"`
	GeneratedFrom string  `yaml:"generated_from,omitempty"`
	Tables        []Table `yaml:"tables"`

	byName map[string]*Table
}

// Load parses the catalog embedded at build time.
func Load() (*Catalog, error) { return Parse(embedded) }

// Parse reads a catalog from YAML and validates its internal consistency.
func Parse(raw []byte) (*Catalog, error) {
	var c Catalog
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode catalog: %w", err)
	}
	c.index()
	if errs := c.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("invalid catalog: %s", joinErrs(errs))
	}
	return &c, nil
}

func (c *Catalog) index() {
	c.byName = make(map[string]*Table, len(c.Tables))
	for i := range c.Tables {
		c.byName[c.Tables[i].Name] = &c.Tables[i]
	}
}

// Table returns the entry for name.
func (c *Catalog) Table(name string) (*Table, bool) {
	t, ok := c.byName[name]
	return t, ok
}

// Names returns every catalogued table name, sorted.
func (c *Catalog) Names() []string {
	out := make([]string, 0, len(c.Tables))
	for _, t := range c.Tables {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// Validate checks the catalog against itself: known classes, the fields each
// class requires, no duplicates, and parent links that resolve.
func (c *Catalog) Validate() []error {
	var errs []error
	if c.SchemaVersion != 1 {
		errs = append(errs, fmt.Errorf("schema_version %d is unsupported; this build understands 1", c.SchemaVersion))
	}
	if strings.TrimSpace(c.ApiaryCompat) == "" {
		errs = append(errs, fmt.Errorf("apiary_compat is required"))
	}
	if len(c.Tables) == 0 {
		errs = append(errs, fmt.Errorf("catalog declares no tables"))
	}
	seen := map[string]bool{}
	for _, t := range c.Tables {
		where := "table " + t.Name
		if strings.TrimSpace(t.Name) == "" {
			errs = append(errs, fmt.Errorf("a table entry has no name"))
			continue
		}
		if seen[t.Name] {
			errs = append(errs, fmt.Errorf("%s: duplicate entry", where))
		}
		seen[t.Name] = true
		if _, ok := classes[t.Class]; !ok {
			errs = append(errs, fmt.Errorf("%s: unknown class %q", where, t.Class))
			continue
		}
		if len(t.Key) == 0 {
			errs = append(errs, fmt.Errorf("%s: key is required — it is the upsert conflict target", where))
		}
		switch t.Class {
		case ClassAppendOnly, ClassMutable:
			if t.Cursor == nil {
				errs = append(errs, fmt.Errorf("%s: class %s requires a cursor", where, t.Class))
			}
		case ClassOpenRow:
			if t.Cursor == nil {
				errs = append(errs, fmt.Errorf("%s: class open_row requires a cursor for newly inserted rows", where))
			}
			if t.State == nil {
				errs = append(errs, fmt.Errorf("%s: class open_row requires a state column — it is what bounds the rescan", where))
			} else if len(t.State.Terminal) == 0 {
				errs = append(errs, fmt.Errorf("%s: state.terminal is empty, so every row would be rescanned forever", where))
			}
		case ClassFollowParent:
			if t.Parent == nil {
				errs = append(errs, fmt.Errorf("%s: class follow_parent requires a parent", where))
			}
			if t.Cursor != nil {
				errs = append(errs, fmt.Errorf("%s: class follow_parent must not declare a cursor; it follows its parent", where))
			}
		case ClassSnapshot:
			if t.Cursor != nil {
				errs = append(errs, fmt.Errorf("%s: class snapshot must not declare a cursor", where))
			}
		}
		if t.Cursor != nil {
			if strings.TrimSpace(t.Cursor.Column) == "" {
				errs = append(errs, fmt.Errorf("%s: cursor.column is required", where))
			}
			if t.Cursor.Kind != CursorInteger && t.Cursor.Kind != CursorTimestamp {
				errs = append(errs, fmt.Errorf("%s: cursor.kind %q must be integer or timestamp", where, t.Cursor.Kind))
			}
		}
		if t.TimeColumn != "" && len(t.Timestamps) > 0 && !containsStr(t.Timestamps, t.TimeColumn) {
			errs = append(errs, fmt.Errorf("%s: time_column %q is not listed in timestamps", where, t.TimeColumn))
		}
		if t.State != nil && strings.TrimSpace(t.State.Column) == "" {
			errs = append(errs, fmt.Errorf("%s: state.column is required when state is declared", where))
		}
	}
	// Parent links resolve only once every name is known.
	for _, t := range c.Tables {
		if t.Parent == nil {
			continue
		}
		if t.Parent.Table == t.Name {
			errs = append(errs, fmt.Errorf("table %s: parent refers to itself", t.Name))
			continue
		}
		if _, ok := seen[t.Parent.Table]; !ok {
			errs = append(errs, fmt.Errorf("table %s: parent table %q is not in the catalog", t.Name, t.Parent.Table))
		}
		if t.Parent.Local == "" || t.Parent.Remote == "" {
			errs = append(errs, fmt.Errorf("table %s: parent needs both local and remote columns", t.Name))
		}
	}
	return errs
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func joinErrs(errs []error) string {
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, e.Error())
	}
	return strings.Join(parts, "; ")
}
