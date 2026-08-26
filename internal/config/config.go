// Package config parses and resolves pgsink's configuration.
//
// The interesting part is not parsing but resolution: how a global default and
// a per-table setting combine. Those rules are fixed and deliberate, not
// emergent — see Resolve.
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Source is the Apiary installation to read.
type Source struct {
	// DSN is the path to apiary.db, optionally with a sqlite:// prefix.
	DSN string `yaml:"dsn"`
	// Instance names this Apiary in the target, so several can share a
	// database. It is also what ${source.instance} expands to.
	Instance string `yaml:"instance"`
	// Wake is the daemon's Unix socket. When set, its event stream nudges the
	// sync loop; Sync.Interval remains the fallback.
	Wake string `yaml:"wake,omitempty"`
}

// Target is the PostgreSQL database to write.
type Target struct {
	DSN    string `yaml:"dsn"`
	Schema string `yaml:"schema,omitempty"`
}

// Sync tunes the follow loop.
type Sync struct {
	Interval  string `yaml:"interval,omitempty"`
	BatchSize int    `yaml:"batch_size,omitempty"`
	// Overlap re-reads a window behind the watermark on timestamp cursors,
	// absorbing same-second writes and small clock adjustments. The idempotent
	// upsert makes the re-delivery harmless.
	Overlap string `yaml:"overlap,omitempty"`
}

// Defaults applies to every table unless a table says otherwise.
type Defaults struct {
	Enabled     *bool            `yaml:"enabled,omitempty"`
	ExtraFields map[string]Extra `yaml:"extra_fields,omitempty"`
	// ExcludeColumns drops a column wherever it appears — "never ship
	// input_prompt" is a meaningful thing to say about a whole database.
	ExcludeColumns []string `yaml:"exclude_columns,omitempty"`
	Filters        []Filter `yaml:"filters,omitempty"`
	// Since windows every table by its own time dimension. It exists because a
	// literal global filter cannot: Apiary's tables call their time column
	// created_at, timestamp, checked_at, started_at or registered_at, so any
	// one name would be missing from most of them. The catalog knows which is
	// which; this resolves through it. Tables with no time dimension are not
	// windowed, and Plan.Unwindowed names them rather than leaving it implied.
	Since string `yaml:"since,omitempty"`
	Until string `yaml:"until,omitempty"`
	// IncludeColumns is accepted only so it can be rejected with an
	// explanation. A projection is a statement about one table's columns, and
	// no two Apiary tables share a column set — a global list would omit some
	// table's key or cursor and fail resolution for most of the database.
	IncludeColumns []string `yaml:"include_columns,omitempty"`
}

// TableConfig overrides the defaults for one table.
type TableConfig struct {
	Enabled        *bool            `yaml:"enabled,omitempty"`
	ExtraFields    map[string]Extra `yaml:"extra_fields,omitempty"`
	IncludeColumns []string         `yaml:"include_columns,omitempty"`
	ExcludeColumns []string         `yaml:"exclude_columns,omitempty"`
	Filters        []Filter         `yaml:"filters,omitempty"`
	JSONColumns    []string         `yaml:"json_columns,omitempty"`
	// IgnoreGlobalFilters is the only way to escape a global filter, and it is
	// deliberately verbose. Global filters are guarantees — a tenant scope, a
	// PII exclusion — so defeating one should read like the exception it is.
	IgnoreGlobalFilters bool `yaml:"ignore_global_filters,omitempty"`
}

// File is the whole configuration document.
type File struct {
	Source   Source                 `yaml:"source"`
	Target   Target                 `yaml:"target"`
	Sync     Sync                   `yaml:"sync,omitempty"`
	Defaults Defaults               `yaml:"defaults,omitempty"`
	Tables   map[string]TableConfig `yaml:"tables,omitempty"`
}

// Load reads and validates a configuration file.
func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw)
}

// Parse reads a configuration document from YAML.
func Parse(raw []byte) (*File, error) {
	var f File
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	f.applyDefaults()
	if err := f.expandEnv(); err != nil {
		return nil, err
	}
	if errs := f.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("invalid config:\n  %s", joinErrs(errs, "\n  "))
	}
	return &f, nil
}

// expandEnv resolves ${env:NAME} in the two fields that carry secrets. A
// PostgreSQL DSN holds a password, so it must be possible to keep it out of the
// file — and a missing variable has to fail loudly rather than resolve to an
// empty string that later reads as "dsn is required".
func (f *File) expandEnv() error {
	for _, ref := range []struct {
		name string
		ptr  *string
	}{
		{"target.dsn", &f.Target.DSN},
		{"source.dsn", &f.Source.DSN},
	} {
		expanded, err := expandEnvRefs(*ref.ptr)
		if err != nil {
			return fmt.Errorf("%s: %w", ref.name, err)
		}
		*ref.ptr = expanded
	}
	return nil
}

// expandEnvRefs replaces ${env:NAME} spans, erroring on an unset variable
// rather than substituting an empty string.
func expandEnvRefs(value string) (string, error) {
	var bad []string
	out := placeholder.ReplaceAllStringFunc(value, func(match string) string {
		body := strings.TrimSpace(match[2 : len(match)-1])
		if !strings.HasPrefix(body, "env:") {
			bad = append(bad, fmt.Sprintf("${%s} is not supported here; only ${env:NAME}", body))
			return match
		}
		name := strings.TrimPrefix(body, "env:")
		if !validEnvName(name) {
			bad = append(bad, fmt.Sprintf("${env:%s} is not a valid environment variable name", name))
			return match
		}
		v, ok := os.LookupEnv(name)
		if !ok {
			bad = append(bad, fmt.Sprintf("environment variable %s is not set", name))
			return match
		}
		return v
	})
	if len(bad) > 0 {
		return "", fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	return out, nil
}

func (f *File) applyDefaults() {
	if f.Target.Schema == "" {
		f.Target.Schema = "apiary"
	}
	if f.Sync.Interval == "" {
		f.Sync.Interval = "10s"
	}
	if f.Sync.BatchSize == 0 {
		f.Sync.BatchSize = 2000
	}
	if f.Sync.Overlap == "" {
		f.Sync.Overlap = "30s"
	}
	if f.Defaults.Enabled == nil {
		on := true
		f.Defaults.Enabled = &on
	}
}

// SourcePath strips an optional sqlite:// prefix from the source DSN.
func (f *File) SourcePath() string {
	return strings.TrimPrefix(strings.TrimPrefix(f.Source.DSN, "sqlite://"), "sqlite:")
}

// IntervalDuration returns the parsed poll interval.
func (f *File) IntervalDuration() (time.Duration, error) {
	return parseDur("sync.interval", f.Sync.Interval)
}

// OverlapDuration returns the parsed cursor overlap window.
func (f *File) OverlapDuration() (time.Duration, error) {
	return parseDur("sync.overlap", f.Sync.Overlap)
}

func parseDur(field, value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a duration: %w", field, value, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", field, value)
	}
	return d, nil
}

// Validate checks everything that can be known without a database. Anything
// needing real column names is checked by Check, against a reflected schema.
func (f *File) Validate() []error {
	var errs []error
	if strings.TrimSpace(f.Source.DSN) == "" {
		errs = append(errs, fmt.Errorf("source.dsn is required"))
	}
	if strings.TrimSpace(f.Source.Instance) == "" {
		errs = append(errs, fmt.Errorf("source.instance is required; it identifies this Apiary in the target and cannot be changed later without a reload"))
	}
	if strings.TrimSpace(f.Target.DSN) == "" {
		errs = append(errs, fmt.Errorf("target.dsn is required"))
	}
	if !validIdent(f.Target.Schema) {
		errs = append(errs, fmt.Errorf("target.schema %q is not a valid identifier", f.Target.Schema))
	}
	if f.Sync.BatchSize <= 0 {
		errs = append(errs, fmt.Errorf("sync.batch_size must be positive"))
	}
	if _, err := f.IntervalDuration(); err != nil {
		errs = append(errs, err)
	}
	if _, err := f.OverlapDuration(); err != nil {
		errs = append(errs, err)
	}
	if f.Source.Wake != "" && !strings.HasPrefix(f.Source.Wake, "unix://") {
		errs = append(errs, fmt.Errorf("source.wake %q must be a unix:// socket path; Apiary does not serve its event stream over TCP", f.Source.Wake))
	}

	if len(f.Defaults.IncludeColumns) > 0 {
		errs = append(errs, fmt.Errorf("defaults.include_columns is not supported: a projection is a statement "+
			"about one table's columns, and Apiary's tables share no common set — any global list would omit "+
			"some table's key or cursor column. Set include_columns per table, or use defaults.exclude_columns "+
			"to drop a column wherever it appears"))
	}

	if f.Defaults.Since != "" && f.Defaults.Until != "" && f.Defaults.Since >= f.Defaults.Until {
		errs = append(errs, fmt.Errorf("defaults.since %q is not before defaults.until %q, which would match no rows", f.Defaults.Since, f.Defaults.Until))
	}

	errs = append(errs, validateExtras("defaults", f.Defaults.ExtraFields)...)
	for _, flt := range f.Defaults.Filters {
		if err := flt.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("defaults.filters: %w", err))
		}
	}
	for _, name := range sortedTableNames(f.Tables) {
		tc := f.Tables[name]
		errs = append(errs, validateExtras("tables."+name, tc.ExtraFields)...)
		for _, flt := range tc.Filters {
			if err := flt.Validate(); err != nil {
				errs = append(errs, fmt.Errorf("tables.%s.filters: %w", name, err))
			}
		}
		if tc.IgnoreGlobalFilters && len(f.Defaults.Filters) == 0 {
			errs = append(errs, fmt.Errorf("tables.%s: ignore_global_filters is set but there are no global filters to ignore", name))
		}
	}
	return errs
}

func validateExtras(where string, extras map[string]Extra) []error {
	var errs []error
	for _, name := range sortedExtraNames(extras) {
		if !validIdent(name) {
			errs = append(errs, fmt.Errorf("%s.extra_fields: %q is not a valid column name", where, name))
			continue
		}
		if err := extras[name].Validate(); err != nil {
			errs = append(errs, fmt.Errorf("%s.extra_fields.%s: %w", where, name, err))
		}
	}
	return errs
}

// validIdent accepts the unquoted-identifier shape both SQLite and PostgreSQL
// agree on, so no generated DDL ever needs quoting or escaping.
func validIdent(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
		case r >= 'A' && r <= 'Z':
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

func sortedTableNames(m map[string]TableConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedExtraNames(m map[string]Extra) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinErrs(errs []error, sep string) string {
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, e.Error())
	}
	return strings.Join(parts, sep)
}
