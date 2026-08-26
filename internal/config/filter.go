package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Op is a filter comparison.
type Op string

const (
	OpEq      Op = "eq"
	OpNe      Op = "ne"
	OpLt      Op = "lt"
	OpLte     Op = "lte"
	OpGt      Op = "gt"
	OpGte     Op = "gte"
	OpIn      Op = "in"
	OpNotIn   Op = "not_in"
	OpLike    Op = "like"
	OpIsNull  Op = "is_null"
	OpNotNull Op = "not_null"
)

var ops = map[Op]struct{}{
	OpEq: {}, OpNe: {}, OpLt: {}, OpLte: {}, OpGt: {}, OpGte: {},
	OpIn: {}, OpNotIn: {}, OpLike: {}, OpIsNull: {}, OpNotNull: {},
}

// OpNames lists the supported operators, for error messages.
func OpNames() []string {
	return []string{
		string(OpEq), string(OpGt), string(OpGte), string(OpIn), string(OpIsNull),
		string(OpLike), string(OpLt), string(OpLte), string(OpNe), string(OpNotIn),
		string(OpNotNull),
	}
}

// Filter selects which rows are read.
//
// Filters are not applied after transfer: they compile to a WHERE clause pushed
// into the SQLite read, so excluded rows are never read, never serialised and
// never sent.
type Filter struct {
	Column string `yaml:"column"`
	Op     Op     `yaml:"op"`
	Value  any    `yaml:"value,omitempty"`
	// OrNull widens the comparison to keep rows whose value is NULL. Set only
	// by the since/until window, where an unknown timestamp means the row is
	// still in flight and is the last thing that should be dropped.
	OrNull bool `yaml:"-"`
}

// Validate checks the operator and that the value matches its arity.
func (f Filter) Validate() error {
	if !validIdent(f.Column) {
		return fmt.Errorf("column %q is not a valid column name", f.Column)
	}
	if _, ok := ops[f.Op]; !ok {
		return fmt.Errorf("column %s: unknown op %q; supported: %s", f.Column, f.Op, strings.Join(OpNames(), ", "))
	}
	switch f.Op {
	case OpIsNull, OpNotNull:
		if f.Value != nil {
			return fmt.Errorf("column %s: op %s takes no value", f.Column, f.Op)
		}
	case OpIn, OpNotIn:
		list, ok := f.Value.([]any)
		if !ok {
			return fmt.Errorf("column %s: op %s needs a list value", f.Column, f.Op)
		}
		if len(list) == 0 {
			// An empty IN matches nothing, which silently replicates an empty
			// table. Almost certainly a mistake, and an expensive one to debug.
			return fmt.Errorf("column %s: op %s has an empty list, which would match no rows", f.Column, f.Op)
		}
	default:
		if f.Value == nil {
			return fmt.Errorf("column %s: op %s needs a value", f.Column, f.Op)
		}
		if _, isList := f.Value.([]any); isList {
			return fmt.Errorf("column %s: op %s takes a single value, not a list", f.Column, f.Op)
		}
	}
	return nil
}

// String renders the filter the way it reads in configuration, for diagnostics.
func (f Filter) String() string {
	switch {
	case f.Op == OpIsNull || f.Op == OpNotNull:
		return fmt.Sprintf("%s %s", f.Column, f.Op)
	case f.OrNull:
		return fmt.Sprintf("(%s %s %v or null)", f.Column, f.Op, f.Value)
	default:
		return fmt.Sprintf("%s %s %v", f.Column, f.Op, f.Value)
	}
}

// UnmarshalYAML accepts the mapping form and normalises list values to []any so
// callers never have to care whether the YAML held strings, ints or a mix.
func (f *Filter) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		Column string `yaml:"column"`
		Op     Op     `yaml:"op"`
		Value  any    `yaml:"value"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	f.Column, f.Op, f.Value = raw.Column, raw.Op, raw.Value
	return nil
}
