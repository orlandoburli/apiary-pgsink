package observe

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func TestNotReadyBeforeTheFirstPass(t *testing.T) {
	m := New("laptop", t0)
	ok, reason := m.Healthy()
	if ok {
		t.Error("a sink that has not completed a pass is not ready")
	}
	if !strings.Contains(reason, "no pass") {
		t.Errorf("reason = %q, want it to say why", reason)
	}
}

// A sink with nothing to replicate is healthy. A readiness probe that flaps
// whenever the source is quiet is worse than no probe at all.
func TestAnIdleSinkIsHealthy(t *testing.T) {
	m := New("laptop", t0)
	m.RecordPass(t0, time.Millisecond, nil)
	if ok, reason := m.Healthy(); !ok {
		t.Errorf("an empty pass should be healthy, got %q", reason)
	}
}

func TestAFailedPassMakesTheSinkUnready(t *testing.T) {
	m := New("laptop", t0)
	m.RecordPass(t0, time.Millisecond, nil)
	m.RecordError(t0.Add(time.Second), errors.New("target unreachable"))
	ok, reason := m.Healthy()
	if ok {
		t.Fatal("a failed pass must show as unready")
	}
	if !strings.Contains(reason, "target unreachable") {
		t.Errorf("reason = %q, want the underlying error", reason)
	}
	// Recovering must clear it, or the sink stays unready for its whole life
	// after one transient failure.
	m.RecordPass(t0.Add(2*time.Second), time.Millisecond, nil)
	if ok, reason := m.Healthy(); !ok {
		t.Errorf("a successful pass should clear the failure, got %q", reason)
	}
}

func TestExposeRendersCountersAndPerTableSeries(t *testing.T) {
	m := New("laptop", t0)
	m.RecordPass(t0, 250*time.Millisecond, []Result{
		{Table: "task_executions", Rows: 12, Quarantined: 1},
		{Table: "step_runs", Rows: 30},
	})
	m.RecordLag(map[string]float64{"step_runs": 4.5})
	out := m.Expose(t0.Add(10 * time.Second))

	for _, want := range []string{
		`pgsink_up{instance="laptop"} 1`,
		`pgsink_passes_total{instance="laptop"} 1`,
		`pgsink_rows_written_total{instance="laptop"} 42`,
		`pgsink_rows_quarantined_total{instance="laptop"} 1`,
		`pgsink_last_pass_duration_seconds{instance="laptop"} 0.250`,
		`pgsink_seconds_since_last_pass{instance="laptop"} 10.000`,
		`pgsink_table_rows_written_total{instance="laptop",table="task_executions"} 12`,
		`pgsink_table_rows_quarantined_total{instance="laptop",table="task_executions"} 1`,
		`pgsink_table_lag_seconds{instance="laptop",table="step_runs"} 4.500`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition is missing %q\n%s", want, out)
		}
	}
	// Every series must be preceded by HELP and TYPE, or a scrape rejects it.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "pgsink_") {
			name := line[:strings.IndexAny(line, "{ ")]
			if !strings.Contains(out, "# TYPE "+name+" ") {
				t.Errorf("%s has no TYPE line", name)
			}
		}
	}
}

func TestCountersAccumulateAcrossPasses(t *testing.T) {
	m := New("laptop", t0)
	for i := 0; i < 3; i++ {
		m.RecordPass(t0, time.Millisecond, []Result{{Table: "tasks", Rows: 5}})
	}
	out := m.Expose(t0)
	if !strings.Contains(out, `pgsink_rows_written_total{instance="laptop"} 15`) {
		t.Errorf("counters should accumulate:\n%s", out)
	}
	if !strings.Contains(out, `pgsink_passes_total{instance="laptop"} 3`) {
		t.Errorf("pass counter should accumulate:\n%s", out)
	}
}

// Label values are operator-supplied — source.instance comes from a config file
// — so they have to be escaped or one quote breaks every scrape.
func TestLabelValuesAreEscaped(t *testing.T) {
	m := New(`odd"name\here`, t0)
	m.RecordPass(t0, time.Millisecond, nil)
	out := m.Expose(t0)
	if !strings.Contains(out, `instance="odd\"name\\here"`) {
		t.Errorf("label was not escaped:\n%s", out)
	}
}

// Before any pass, "seconds since the last one" has no honest answer. -1 is
// distinguishable from zero, which would read as "just now".
func TestSecondsSinceLastPassIsNegativeBeforeAnyPass(t *testing.T) {
	out := New("laptop", t0).Expose(t0)
	if !strings.Contains(out, `pgsink_seconds_since_last_pass{instance="laptop"} -1`) {
		t.Errorf("want -1 before the first pass:\n%s", out)
	}
}

func TestConcurrentReadsAndWritesAreSafe(t *testing.T) {
	m := New("laptop", t0)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			m.RecordPass(t0, time.Millisecond, []Result{{Table: "tasks", Rows: 1}})
			m.RecordLag(map[string]float64{"tasks": float64(i)})
		}
		close(done)
	}()
	for i := 0; i < 200; i++ {
		_ = m.Expose(t0)
		_, _ = m.Healthy()
	}
	<-done
}
