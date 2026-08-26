package observe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Serve runs the observability endpoints until ctx ends.
//
//	/metrics   Prometheus exposition
//	/healthz   liveness: the process is up
//	/readyz    readiness: a pass has completed and the last one succeeded
//
// It is a separate listener from anything else the sink does, and it is
// read-only: nothing here can change what is replicated.
func Serve(ctx context.Context, addr string, m *Metrics) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprint(w, m.Expose(time.Now()))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		ready, reason := m.Healthy()
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		fmt.Fprintln(w, reason)
	})

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Addr resolves the address a listener would bind, for reporting before Serve
// blocks.
func Addr(addr string) string {
	if addr == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return addr
	}
	return addr
}
