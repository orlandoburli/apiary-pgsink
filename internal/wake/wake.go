// Package wake listens to an Apiary daemon's event stream so the sync loop can
// react to activity instead of only waiting out its interval.
package wake

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Waker signals when the daemon has recorded an execution event.
//
// It is only a nudge. Every fact the sink replicates is read from the database,
// never from the event payload, so a missed or duplicated signal costs latency
// at worst — never correctness. That is why a failure here degrades to the poll
// interval rather than stopping the sink.
type Waker struct {
	client *http.Client
	url    string
}

// New builds a Waker for a unix:// socket path. Apiary serves its event stream
// over a Unix domain socket rather than TCP, which is also why the sink has to
// run on the daemon's host.
func New(addr string) (*Waker, error) {
	path := strings.TrimPrefix(addr, "unix://")
	if path == "" || path == addr {
		return nil, fmt.Errorf("wake address %q must be a unix:// socket path", addr)
	}
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	return &Waker{
		client: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", path)
			},
		}},
		// The host is ignored for a unix socket but has to be syntactically
		// present for the URL to parse.
		url: "http://apiary/events/stream",
	}, nil
}

// Run streams events until ctx ends, sending one signal per event on ch.
//
// ch is expected to be buffered with capacity one: a burst of a hundred events
// should wake the loop once, not a hundred times, and the pass that follows
// picks up everything they represent.
func (w *Waker) Run(ctx context.Context, ch chan<- struct{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("connect to the event stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("event stream returned %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Only the fact of an event matters. Its contents are the daemon's, and
		// deliberately not trusted as data to replicate.
		if !strings.HasPrefix(scanner.Text(), "data:") {
			continue
		}
		select {
		case ch <- struct{}{}:
		default: // already pending; one wake is enough
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("event stream ended: %w", err)
	}
	return ctx.Err()
}
