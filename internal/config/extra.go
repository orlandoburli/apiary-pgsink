package config

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/orlandoburli/apiary-pgsink/internal/pgtype"
)

// Extra is one injected column: a value template and the PostgreSQL type the
// column is created with.
//
// Two YAML forms are accepted. The shorthand infers the type:
//
//	tenant_id: "acme"                     -> text
//	ingested_at: "${now}"                 -> timestamptz
//
// The long form states it:
//
//	replica_lag_ms: {value: 0, type: bigint}
type Extra struct {
	Value string        `yaml:"value"`
	Type  pgtype.PGType `yaml:"type,omitempty"`
}

// placeholder matches ${...} spans. The inner grammar is checked separately so
// an unknown function can be named in the error rather than silently ignored.
var placeholder = regexp.MustCompile(`\$\{([^}]*)\}`)

// Validate checks the template's placeholders and the declared type.
func (e Extra) Validate() error {
	if e.Type != "" {
		if _, err := pgtype.ParsePGType(string(e.Type)); err != nil {
			return err
		}
	}
	// An unclosed placeholder would otherwise be written verbatim into every
	// row — a silent, permanent typo.
	if i := strings.Index(e.Value, "${"); i >= 0 {
		rest := e.Value[i:]
		if !strings.Contains(rest, "}") {
			return fmt.Errorf("unclosed placeholder in %q", e.Value)
		}
	}
	for _, m := range placeholder.FindAllStringSubmatch(e.Value, -1) {
		if err := validatePlaceholder(m[1]); err != nil {
			return err
		}
	}
	return nil
}

// validatePlaceholder checks one ${...} body against the deliberately small
// substitution language. There is no expression evaluator: the moment this
// grows conditionals it needs its own error reporting and its own tests, and it
// adds nothing a computed view in PostgreSQL cannot do better.
func validatePlaceholder(body string) error {
	body = strings.TrimSpace(body)
	switch {
	case body == "now", body == "table", body == "source.instance":
		return nil
	case strings.HasPrefix(body, "env:"):
		name := strings.TrimPrefix(body, "env:")
		if !validEnvName(name) {
			return fmt.Errorf("${env:%s} is not a valid environment variable name", name)
		}
		return nil
	case strings.HasPrefix(body, "row."):
		col := strings.TrimPrefix(body, "row.")
		if !validIdent(col) {
			return fmt.Errorf("${row.%s} is not a valid column name", col)
		}
		return nil
	default:
		return fmt.Errorf("unknown placeholder ${%s}; supported: ${now}, ${table}, ${source.instance}, ${env:NAME}, ${row.column}", body)
	}
}

func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// ResolvedType returns the declared type, or the one inferred from the template.
//
// Inference is intentionally shallow: ${now} is a timestamp and everything else
// is text. Guessing that "123" means bigint would make a column's type depend on
// the first value someone happened to write, and change under them later.
func (e Extra) ResolvedType() pgtype.PGType {
	if e.Type != "" {
		return e.Type
	}
	if strings.TrimSpace(e.Value) == "${now}" {
		return pgtype.TimestampTZ
	}
	return pgtype.Text
}

// References reports whether the template reads a source column, which decides
// whether the value can be computed once per batch or must be per row.
func (e Extra) References() []string {
	var cols []string
	for _, m := range placeholder.FindAllStringSubmatch(e.Value, -1) {
		if body := strings.TrimSpace(m[1]); strings.HasPrefix(body, "row.") {
			cols = append(cols, strings.TrimPrefix(body, "row."))
		}
	}
	return cols
}

// UnmarshalYAML accepts both the scalar shorthand and the {value, type} form.
func (e *Extra) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var scalar any
		if err := node.Decode(&scalar); err != nil {
			return err
		}
		e.Value = fmt.Sprintf("%v", scalar)
		return nil
	}
	var long struct {
		Value any    `yaml:"value"`
		Type  string `yaml:"type"`
	}
	if err := node.Decode(&long); err != nil {
		return err
	}
	if long.Value == nil {
		return fmt.Errorf("extra field needs a value")
	}
	e.Value = fmt.Sprintf("%v", long.Value)
	e.Type = pgtype.PGType(strings.ToLower(strings.TrimSpace(long.Type)))
	return nil
}
