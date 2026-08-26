// Package observe exposes what the sink is doing, in Prometheus text format.
//
// The exposition is hand-written rather than pulled from a client library. It
// is a few dozen lines, it keeps the binary's dependency list to pgx, the SQLite
// driver and YAML, and there is nothing here — no histograms, no exemplars —
// that earns a library.
package observe

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// TableStat is the running state of one replicated table.
type TableStat struct {
	Rows        int64
	Quarantined int64
	LastRows    int64
	Skipped     bool
}

// Metrics is the sink's observable state. Safe for concurrent use: the HTTP
// handler reads it while the sync loop writes it.
type Metrics struct {
	mu sync.RWMutex

	instance string
	started  time.Time

	passes      int64
	passErrors  int64
	rows        int64
	quarantined int64

	lastPassAt       time.Time
	lastPassDuration time.Duration
	lastErrorAt      time.Time
	lastError        string

	tables map[string]*TableStat
	// lag is seconds between a table's watermark and now, which is the number
	// an operator actually wants: how far behind the target is.
	lag map[string]float64
}

// New starts a metrics registry for one source instance.
func New(instance string, now time.Time) *Metrics {
	return &Metrics{
		instance: instance,
		started:  now,
		tables:   map[string]*TableStat{},
		lag:      map[string]float64{},
	}
}

// Healthy reports whether the sink is doing its job: it has completed a pass,
// and the most recent one did not fail.
//
// Deliberately not "the last pass found rows". A sink with nothing to do is
// perfectly healthy, and a readiness probe that flaps whenever the source is
// quiet is worse than no probe.
func (m *Metrics) Healthy() (bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.passes == 0 {
		return false, "no pass has completed yet"
	}
	if !m.lastErrorAt.IsZero() && m.lastErrorAt.After(m.lastPassAt) {
		return false, "last pass failed: " + m.lastError
	}
	return true, "ok"
}

// RecordPass folds one successful pass into the totals.
func (m *Metrics) RecordPass(at time.Time, elapsed time.Duration, results []Result) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.passes++
	m.lastPassAt = at
	m.lastPassDuration = elapsed
	for _, r := range results {
		stat, ok := m.tables[r.Table]
		if !ok {
			stat = &TableStat{}
			m.tables[r.Table] = stat
		}
		stat.Rows += r.Rows
		stat.Quarantined += r.Quarantined
		stat.LastRows = r.Rows
		stat.Skipped = r.Skipped
		m.rows += r.Rows
		m.quarantined += r.Quarantined
	}
}

// Result is one table's contribution to a pass.
type Result struct {
	Table       string
	Rows        int64
	Quarantined int64
	Skipped     bool
}

// RecordError notes a failed pass.
func (m *Metrics) RecordError(at time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.passErrors++
	m.lastErrorAt = at
	m.lastError = err.Error()
}

// RecordLag sets how far behind each table's watermark is, in seconds.
func (m *Metrics) RecordLag(lag map[string]float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for table, seconds := range lag {
		m.lag[table] = seconds
	}
}

// Expose renders the Prometheus text exposition format.
func (m *Metrics) Expose(now time.Time) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var b strings.Builder
	inst := escape(m.instance)

	metric := func(name, help, typ string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	global := func(name string, value any) {
		fmt.Fprintf(&b, "%s{instance=\"%s\"} %v\n", name, inst, value)
	}

	metric("pgsink_up", "1 when the sink has completed a pass and the last one succeeded.", "gauge")
	healthy, _ := m.healthyLocked()
	global("pgsink_up", boolValue(healthy))

	metric("pgsink_uptime_seconds", "Seconds since the sink started.", "gauge")
	global("pgsink_uptime_seconds", seconds(now.Sub(m.started)))

	metric("pgsink_passes_total", "Sync passes completed.", "counter")
	global("pgsink_passes_total", m.passes)

	metric("pgsink_pass_errors_total", "Sync passes that failed.", "counter")
	global("pgsink_pass_errors_total", m.passErrors)

	metric("pgsink_rows_written_total", "Rows upserted into the target.", "counter")
	global("pgsink_rows_written_total", m.rows)

	metric("pgsink_rows_quarantined_total", "Rows the target refused, filed for review.", "counter")
	global("pgsink_rows_quarantined_total", m.quarantined)

	metric("pgsink_last_pass_duration_seconds", "How long the most recent pass took.", "gauge")
	global("pgsink_last_pass_duration_seconds", seconds(m.lastPassDuration))

	metric("pgsink_seconds_since_last_pass", "Seconds since the most recent pass completed.", "gauge")
	if m.lastPassAt.IsZero() {
		global("pgsink_seconds_since_last_pass", -1)
	} else {
		global("pgsink_seconds_since_last_pass", seconds(now.Sub(m.lastPassAt)))
	}

	names := make([]string, 0, len(m.tables))
	for t := range m.tables {
		names = append(names, t)
	}
	sort.Strings(names)

	metric("pgsink_table_rows_written_total", "Rows upserted per table.", "counter")
	for _, t := range names {
		fmt.Fprintf(&b, "pgsink_table_rows_written_total{instance=\"%s\",table=\"%s\"} %d\n", inst, escape(t), m.tables[t].Rows)
	}
	metric("pgsink_table_rows_quarantined_total", "Rows refused per table.", "counter")
	for _, t := range names {
		fmt.Fprintf(&b, "pgsink_table_rows_quarantined_total{instance=\"%s\",table=\"%s\"} %d\n", inst, escape(t), m.tables[t].Quarantined)
	}

	if len(m.lag) > 0 {
		lagNames := make([]string, 0, len(m.lag))
		for t := range m.lag {
			lagNames = append(lagNames, t)
		}
		sort.Strings(lagNames)
		metric("pgsink_table_lag_seconds",
			"Seconds between a table's replicated watermark and now. The number to alert on.", "gauge")
		for _, t := range lagNames {
			fmt.Fprintf(&b, "pgsink_table_lag_seconds{instance=\"%s\",table=\"%s\"} %.3f\n", inst, escape(t), m.lag[t])
		}
	}
	return b.String()
}

func (m *Metrics) healthyLocked() (bool, string) {
	if m.passes == 0 {
		return false, "no pass has completed yet"
	}
	if !m.lastErrorAt.IsZero() && m.lastErrorAt.After(m.lastPassAt) {
		return false, "last pass failed: " + m.lastError
	}
	return true, "ok"
}

func boolValue(b bool) int {
	if b {
		return 1
	}
	return 0
}

func seconds(d time.Duration) string { return fmt.Sprintf("%.3f", d.Seconds()) }

// escape quotes a label value per the exposition format.
func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}
