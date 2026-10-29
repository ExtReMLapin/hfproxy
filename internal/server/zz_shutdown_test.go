// This file is named to sort last: ListenAndServe shuts the process-wide trace
// provider down when it returns, which the tracing tests (earlier in the
// alphabet) cannot survive.
package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func shutdownTestServer(handler http.Handler) (*Server, *http.Server, net.Listener, *lockedBuffer) {
	logs := &lockedBuffer{}
	s := &Server{metrics: newMetrics(), logger: slog.New(slog.NewTextHandler(logs, nil))}
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	return s, &http.Server{Handler: handler}, listener, logs
}

func TestDrainLetsRequestsInProgressFinish(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	s, server, listener, logs := shutdownTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		io.WriteString(w, "done")
	}))
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + listener.Addr().String())
		if err != nil {
			body <- err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()
	<-started
	drained := make(chan struct{})
	go func() { s.drain(server, nil, 10*time.Second); close(drained) }()
	select {
	case <-drained:
		t.Fatal("drain returned while a request was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-drained
	if got := <-body; got != "done" {
		t.Fatalf("the request in progress got %q, want it to finish", got)
	}
	if err := <-served; err != http.ErrServerClosed {
		t.Fatalf("Serve = %v", err)
	}
	if out := logs.String(); strings.Contains(out, "cut") || !strings.Contains(out, "shutdown complete") {
		t.Fatalf("unexpected log:\n%s", out)
	}
}

func TestDrainCutsRequestsAfterTheGraceAndSaysHowMany(t *testing.T) {
	started := make(chan struct{})
	s, server, listener, logs := shutdownTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	handler := server.Handler
	server.Handler = s.metrics.middlewareDynamic(func() string { return "" }, handler)
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	go func() {
		if resp, err := http.Get("http://" + listener.Addr().String()); err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	begin := time.Now()
	s.drain(server, nil, 150*time.Millisecond)
	if elapsed := time.Since(begin); elapsed > 5*time.Second {
		t.Fatalf("drain took %s with a 150ms grace", elapsed)
	}
	if err := <-served; err != http.ErrServerClosed {
		t.Fatalf("Serve = %v", err)
	}
	if out := logs.String(); !strings.Contains(out, "requests_cut=1") {
		t.Fatalf("the log does not say one request was cut:\n%s", out)
	}
}

// A stop asked for by the operator is a success for the process, whether or
// not requests had to be cut: it must not be reported as an error (exit 1).
func TestListenAndServeReturnsNilAfterAStop(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()
	cfg := auditAPIConfig()
	cfg.ListenAddr = addr
	cfg.ShutdownGrace = time.Second
	s := auditAPINewServer(t, auditAPIFreshStore(t), cfg)
	s.setupOut = io.Discard
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx) }()
	for i := 0; ; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		if i > 200 {
			t.Fatal("the server never listened")
		}
		time.Sleep(25 * time.Millisecond)
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe after a stop = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ListenAndServe did not return after the stop")
	}
}
