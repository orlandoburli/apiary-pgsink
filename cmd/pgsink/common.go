package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"

	"github.com/orlandoburli/apiary-pgsink/internal/catalog"
	"github.com/orlandoburli/apiary-pgsink/internal/config"
	sqlitesrc "github.com/orlandoburli/apiary-pgsink/internal/source/sqlite"
	"github.com/orlandoburli/apiary-pgsink/internal/target"
)

// session is everything a command needs after the configuration has been
// resolved against a real Apiary database.
type session struct {
	File   *config.File
	Plan   *config.Plan
	Source *sql.DB
	Live   catalog.LiveSchema
}

func (s *session) Close() {
	if s.Source != nil {
		s.Source.Close()
	}
}

// openSession loads the configuration, opens the Apiary database read-only,
// reflects it, and resolves the plan against it.
//
// Resolution happens against the live schema every time rather than being
// trusted from the file, so a configuration that no longer fits the database is
// caught before any writing starts, not part-way through.
func openSession(ctx context.Context, configPath string, tableFilter []string) (*session, error) {
	file, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	cat, err := catalog.Load()
	if err != nil {
		return nil, err
	}
	plan, errs := config.Resolve(file, cat)
	if len(errs) > 0 {
		return nil, fmt.Errorf("configuration:\n  %s", joinErrs(errs))
	}

	db, err := sqlitesrc.Open(ctx, file.SourcePath())
	if err != nil {
		return nil, err
	}
	live, err := sqlitesrc.Reflect(ctx, db)
	if err != nil {
		db.Close()
		return nil, err
	}
	if errs := plan.Check(live); len(errs) > 0 {
		db.Close()
		return nil, fmt.Errorf("configuration does not fit this Apiary database:\n  %s", joinErrs(errs))
	}
	if len(tableFilter) > 0 {
		if err := restrict(plan, tableFilter); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &session{File: file, Plan: plan, Source: db, Live: live}, nil
}

// restrict narrows the plan to the named tables, rejecting a name that is not
// in it — a misspelt --tables must not quietly do nothing.
func restrict(plan *config.Plan, names []string) error {
	wanted := map[string]bool{}
	for _, n := range names {
		wanted[n] = true
	}
	var kept []config.Table
	for _, t := range plan.Tables {
		if wanted[t.Name] {
			kept = append(kept, t)
			delete(wanted, t.Name)
		}
	}
	for n := range wanted {
		return fmt.Errorf("--tables names %q, which is not an enabled table in this configuration", n)
	}
	plan.Tables = kept
	return nil
}

// openTarget connects to PostgreSQL and makes sure the schema and watermark
// table exist.
func openTarget(ctx context.Context, file *config.File) (*target.DB, error) {
	db, err := target.Open(ctx, file.Target.DSN, file.Target.Schema)
	if err != nil {
		return nil, err
	}
	if err := db.EnsureSchema(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// plannedChanges computes the DDL needed to make the target fit the plan.
func plannedChanges(ctx context.Context, s *session, db *target.DB) ([]target.Change, error) {
	var all []target.Change
	for _, planned := range s.Plan.Tables {
		live, ok := s.Live[planned.Name]
		if !ok {
			continue // absent from this Apiary version; nothing to create
		}
		want, err := target.Desired(planned, live)
		if err != nil {
			return nil, err
		}
		have, err := db.Reflect(ctx, planned.Name)
		if err != nil {
			return nil, err
		}
		all = append(all, target.Diff(db.Schema(), want, have)...)
	}
	return all, nil
}

func joinErrs(errs []error) string {
	out := ""
	for i, e := range errs {
		if i > 0 {
			out += "\n  "
		}
		out += e.Error()
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func writeln(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
}
