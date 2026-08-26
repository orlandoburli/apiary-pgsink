package config

import (
	"strings"
	"testing"
)

func TestParseAppliesDefaults(t *testing.T) {
	f, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Target.Schema != "apiary" {
		t.Errorf("schema = %q, want apiary", f.Target.Schema)
	}
	if f.Sync.BatchSize != 2000 {
		t.Errorf("batch_size = %d, want 2000", f.Sync.BatchSize)
	}
	if d, _ := f.IntervalDuration(); d.String() != "10s" {
		t.Errorf("interval = %v, want 10s", d)
	}
	if d, _ := f.OverlapDuration(); d.String() != "30s" {
		t.Errorf("overlap = %v, want 30s", d)
	}
	if f.SourcePath() != "/tmp/apiary.db" {
		t.Errorf("SourcePath = %q, want the sqlite:// prefix stripped", f.SourcePath())
	}
}

func TestParseRejectsBadDocuments(t *testing.T) {
	cases := map[string]string{
		"no source dsn":   "source: {instance: x}\ntarget: {dsn: p}\n",
		"no instance":     "source: {dsn: a.db}\ntarget: {dsn: p}\n",
		"no target dsn":   "source: {dsn: a.db, instance: x}\ntarget: {}\n",
		"bad schema name": minimal + "target: {dsn: p, schema: \"drop table\"}\n",
		"bad interval":    minimal + "sync: {interval: soon}\n",
		"zero interval":   minimal + "sync: {interval: 0s}\n",
		"negative batch":  minimal + "sync: {batch_size: -1}\n",
		"unknown field":   minimal + "colour: blue\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(raw)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// Apiary does not serve its event stream over TCP, so an http:// wake address
// would simply never connect. Say so at load time.
func TestWakeMustBeAUnixSocket(t *testing.T) {
	_, err := Parse([]byte(`
source: {dsn: a.db, instance: x, wake: "http://localhost:9000"}
target: {dsn: "postgres://localhost/apiary"}
`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "unix://") {
		t.Errorf("error should name the expected form: %v", err)
	}
}

// A PostgreSQL DSN carries a password, so it has to be possible to keep it out
// of the file — and an unset variable must fail loudly, not resolve to an empty
// string that surfaces later as the confusing "target.dsn is required".
func TestEnvExpansionInSecretBearingFields(t *testing.T) {
	t.Setenv("PG", "postgres://user:pw@localhost/apiary")
	f, err := Parse([]byte("source: {dsn: a.db, instance: x}\ntarget: {dsn: \"${env:PG}\"}\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Target.DSN != "postgres://user:pw@localhost/apiary" {
		t.Errorf("dsn = %q, want the expanded value", f.Target.DSN)
	}

	_, err = Parse([]byte("source: {dsn: a.db, instance: x}\ntarget: {dsn: \"${env:DEFINITELY_NOT_SET_XYZ}\"}\n"))
	if err == nil {
		t.Fatal("an unset variable must be an error")
	}
	if !strings.Contains(err.Error(), "DEFINITELY_NOT_SET_XYZ") {
		t.Errorf("error should name the variable: %v", err)
	}
}

func TestFilterValidation(t *testing.T) {
	cases := map[string]Filter{
		"unknown op":        {Column: "state", Op: "approximately", Value: "x"},
		"in without a list": {Column: "state", Op: OpIn, Value: "done"},
		"eq with a list":    {Column: "state", Op: OpEq, Value: []any{"a", "b"}},
		"missing value":     {Column: "state", Op: OpEq},
		"is_null w/ value":  {Column: "state", Op: OpIsNull, Value: "x"},
		"bad column":        {Column: "state; drop table x", Op: OpEq, Value: "y"},
		// An empty IN matches no rows, so the table replicates as empty — a
		// mistake that looks exactly like "there was no data".
		"empty in list": {Column: "state", Op: OpIn, Value: []any{}},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			if err := f.Validate(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	for _, f := range []Filter{
		{Column: "state", Op: OpIn, Value: []any{"done", "failed"}},
		{Column: "created_at", Op: OpGte, Value: "2026-01-01"},
		{Column: "finished_at", Op: OpNotNull},
		{Column: "cost_usd", Op: OpGt, Value: 0},
	} {
		if err := f.Validate(); err != nil {
			t.Errorf("%s should be valid: %v", f, err)
		}
	}
}

func TestExtraFieldValidation(t *testing.T) {
	bad := map[string]Extra{
		"unknown placeholder": {Value: "${yesterday}"},
		"unclosed":            {Value: "${now"},
		"bad env name":        {Value: "${env:not-upper}"},
		"bad row column":      {Value: "${row.a-b}"},
		"unsupported type":    {Value: "x", Type: "money"},
	}
	for name, e := range bad {
		t.Run(name, func(t *testing.T) {
			if err := e.Validate(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	for _, e := range []Extra{
		{Value: "acme"},
		{Value: "${now}"},
		{Value: "${table}"},
		{Value: "${source.instance}"},
		{Value: "${env:DEPLOY_ENV}"},
		{Value: "${row.runner}"},
		{Value: "${source.instance}/${row.agent_id}"},
	} {
		if err := e.Validate(); err != nil {
			t.Errorf("%q should be valid: %v", e.Value, err)
		}
	}
}

func TestExtraFieldReferences(t *testing.T) {
	e := Extra{Value: "${row.agent_id}-${row.runner}-${now}"}
	got := e.References()
	if len(got) != 2 || got[0] != "agent_id" || got[1] != "runner" {
		t.Errorf("References = %v, want [agent_id runner]", got)
	}
	if refs := (Extra{Value: "static"}).References(); len(refs) != 0 {
		t.Errorf("References = %v, want none", refs)
	}
}

// An unknown placeholder must be an error, not silently written into every row
// as a literal — that is a typo you would find months later in the data.
func TestUnknownPlaceholderNamesItself(t *testing.T) {
	err := Extra{Value: "${yesterday}"}.Validate()
	if err == nil || !strings.Contains(err.Error(), "yesterday") {
		t.Fatalf("error should name the placeholder, got %v", err)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "postgres://localhost/apiary")
	t.Setenv("DEPLOY_ENV", "prod")
	f, err := Load("../../examples/pgsink.yaml")
	if err != nil {
		t.Fatalf("the shipped example must parse: %v", err)
	}
	if f.Source.Instance == "" {
		t.Error("the example should show an instance name")
	}
}
