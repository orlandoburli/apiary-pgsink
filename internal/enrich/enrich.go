// Package enrich evaluates the extra-field templates.
package enrich

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/orlandoburli/apiary-pgsink/internal/config"
	"github.com/orlandoburli/apiary-pgsink/internal/pgtype"
)

var placeholder = regexp.MustCompile(`\$\{([^}]*)\}`)

// Context is everything a template can read that does not come from the row.
type Context struct {
	Instance string
	Table    string
	Now      time.Time
}

// Field is one prepared extra field.
type Field struct {
	Name string
	Type pgtype.PGType
	// Constant is the value when the template reads no row columns, computed
	// once for the whole run.
	Constant any
	// Template is retained only when the value varies per row.
	Template string
	PerRow   bool
}

// Prepare resolves everything that does not depend on a row, once. A run
// injecting four fields into a million rows should not evaluate ${now} and
// ${env:...} a million times, and ${now} in particular must be one timestamp
// for the whole batch rather than drifting down the file.
func Prepare(fields []config.ExtraField, ctx Context) ([]Field, error) {
	out := make([]Field, 0, len(fields))
	for _, f := range fields {
		prepared := Field{Name: f.Name, Type: f.ResolvedType()}
		if refs := f.References(); len(refs) > 0 {
			prepared.PerRow = true
			prepared.Template = f.Value
			out = append(out, prepared)
			continue
		}
		value, err := expand(f.Value, ctx, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("extra field %s: %w", f.Name, err)
		}
		prepared.Constant = coerce(value, prepared.Type, ctx.Now)
		out = append(out, prepared)
	}
	return out, nil
}

// Value returns the field's value for one row.
func (f Field) Value(ctx Context, columns []string, row []any) (any, error) {
	if !f.PerRow {
		return f.Constant, nil
	}
	s, err := expand(f.Template, ctx, columns, row)
	if err != nil {
		return nil, fmt.Errorf("extra field %s: %w", f.Name, err)
	}
	return coerce(s, f.Type, ctx.Now), nil
}

func expand(template string, ctx Context, columns []string, row []any) (string, error) {
	var failure error
	out := placeholder.ReplaceAllStringFunc(template, func(match string) string {
		body := strings.TrimSpace(match[2 : len(match)-1])
		switch {
		case body == "now":
			return ctx.Now.UTC().Format(time.RFC3339Nano)
		case body == "table":
			return ctx.Table
		case body == "source.instance":
			return ctx.Instance
		case strings.HasPrefix(body, "env:"):
			// Validated at load time, so an unset variable here means the
			// environment changed under a running process. Empty is the honest
			// answer; failing the batch would be worse.
			return os.Getenv(strings.TrimPrefix(body, "env:"))
		case strings.HasPrefix(body, "row."):
			name := strings.TrimPrefix(body, "row.")
			for i, c := range columns {
				if c == name {
					return render(row[i])
				}
			}
			failure = fmt.Errorf("column %q is not in this row", name)
			return match
		default:
			failure = fmt.Errorf("unknown placeholder ${%s}", body)
			return match
		}
	})
	return out, failure
}

func render(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// coerce converts a rendered template to what the declared column type needs.
// Only timestamptz needs it: text columns take the string, and any other type
// was declared deliberately and is handed over for the driver to parse.
func coerce(s string, t pgtype.PGType, now time.Time) any {
	if t != pgtype.TimestampTZ {
		return s
	}
	if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return parsed
	}
	return now.UTC()
}
