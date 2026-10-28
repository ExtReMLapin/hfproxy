package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestCircuitBreakerStateMachine(t *testing.T) {
	const threshold = 3
	cooldown := 10 * time.Second
	start := time.Unix(1_000_000, 0)
	b := &circuitBreaker{}

	for i := 0; i < threshold-1; i++ {
		if !b.Allow(start, threshold) {
			t.Fatalf("closed circuit refused request %d", i)
		}
		if b.Failure(start, threshold, cooldown) {
			t.Fatalf("circuit opened after only %d failures", i+1)
		}
	}
	b.Success()
	for i := 0; i < threshold-1; i++ {
		b.Failure(start, threshold, cooldown)
	}
	if !b.Allow(start, threshold) {
		t.Fatal("a success must reset the failure count")
	}
	if !b.Failure(start, threshold, cooldown) {
		t.Fatal("the failure reaching the threshold must open the circuit")
	}
	if b.Allow(start.Add(cooldown-time.Second), threshold) {
		t.Fatal("an open circuit let a request through during the cooldown")
	}

	probeTime := start.Add(cooldown)
	if !b.Allow(probeTime, threshold) {
		t.Fatal("no probe was allowed after the cooldown")
	}
	if b.Allow(probeTime, threshold) {
		t.Fatal("a second request passed while the probe was running")
	}
	if !b.Failure(probeTime, threshold, cooldown) {
		t.Fatal("a failed probe must reopen the circuit")
	}
	if b.Allow(probeTime.Add(cooldown-time.Second), threshold) {
		t.Fatal("the circuit did not wait a full cooldown after a failed probe")
	}

	secondProbe := probeTime.Add(cooldown)
	if !b.Allow(secondProbe, threshold) {
		t.Fatal("no second probe was allowed")
	}
	b.Abort()
	if !b.Allow(secondProbe, threshold) {
		t.Fatal("an aborted probe must let another probe through")
	}
	b.Success()
	for i := 0; i < 10; i++ {
		if !b.Allow(secondProbe, threshold) {
			t.Fatal("a successful probe must close the circuit")
		}
	}
}

func TestCircuitBreakerDisabledWithZeroThreshold(t *testing.T) {
	b := &circuitBreaker{}
	for i := 0; i < 100; i++ {
		if b.Failure(time.Now(), 0, time.Minute) || !b.Allow(time.Now(), 0) {
			t.Fatal("a disabled breaker must never open")
		}
	}
}

func TestFetchHubFailsFastWhileTheCircuitIsOpen(t *testing.T) {
	previous := upstreamRetryBase
	upstreamRetryBase = time.Millisecond
	t.Cleanup(func() { upstreamRetryBase = previous })

	var requests atomic.Int32
	var down atomic.Bool
	down.Store(true)
	env := ahubNewEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	state := *env.server.runtime.Load()
	state.config.UpstreamBreakerThreshold = 3
	state.config.UpstreamBreakerCooldown = 250 * time.Millisecond
	env.server.runtime.Store(&state)

	fetch := func() (int, error) {
		resp, _, err := env.server.fetchHub(context.Background(), http.MethodGet, "/api/models/org/m", nil, nil)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		return resp.StatusCode, nil
	}

	for i := 0; i < 3; i++ {
		if status, err := fetch(); err != nil || status != http.StatusServiceUnavailable {
			t.Fatalf("failure %d: status=%d err=%v", i, status, err)
		}
	}
	before := requests.Load()
	started := time.Now()
	if _, err := fetch(); !errors.Is(err, errUpstreamCircuitOpen) {
		t.Fatalf("with the circuit open, fetchHub returned %v, want errUpstreamCircuitOpen", err)
	}
	if requests.Load() != before || time.Since(started) > 100*time.Millisecond {
		t.Fatalf("an open circuit still reached the Hub (%d new requests) or was slow (%v)", requests.Load()-before, time.Since(started))
	}

	time.Sleep(300 * time.Millisecond)
	if _, err := fetch(); errors.Is(err, errUpstreamCircuitOpen) {
		t.Fatal("no probe was allowed after the cooldown")
	}
	if _, err := fetch(); !errors.Is(err, errUpstreamCircuitOpen) {
		t.Fatalf("a failed probe must reopen the circuit, got %v", err)
	}

	down.Store(false)
	time.Sleep(300 * time.Millisecond)
	if status, err := fetch(); err != nil || status != http.StatusOK {
		t.Fatalf("probe after recovery: status=%d err=%v", status, err)
	}
	for i := 0; i < 5; i++ {
		if status, err := fetch(); err != nil || status != http.StatusOK {
			t.Fatalf("after the circuit closed, request %d: status=%d err=%v", i, status, err)
		}
	}
}

func TestClientErrorsDoNotOpenTheCircuit(t *testing.T) {
	var requests atomic.Int32
	env := ahubNewEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	state := *env.server.runtime.Load()
	state.config.UpstreamBreakerThreshold = 2
	state.config.UpstreamBreakerCooldown = time.Minute
	env.server.runtime.Store(&state)
	for i := 0; i < 10; i++ {
		resp, _, err := env.server.fetchHub(context.Background(), http.MethodGet, "/api/models/org/missing", nil, nil)
		if err != nil {
			t.Fatalf("request %d: %v (a 404 must not trip the breaker)", i, err)
		}
		resp.Body.Close()
	}
	if requests.Load() != 10 {
		t.Fatalf("Hub saw %d requests, want 10", requests.Load())
	}
}

func TestFetchHubReturnsNotModifiedAsAnAnswer(t *testing.T) {
	var sawConditional atomic.Bool
	env := ahubNewEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawConditional.Store(r.Header.Get("If-None-Match") == `"v1"`)
		w.WriteHeader(http.StatusNotModified)
	}))
	resp, _, err := env.server.fetchHub(context.Background(), http.MethodGet, "/api/models/org/m", nil, http.Header{"If-None-Match": {`"v1"`}})
	if err != nil {
		t.Fatalf("a 304 from the Hub is an answer, not a redirect: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified || !sawConditional.Load() {
		t.Fatalf("status=%d conditional header forwarded=%v", resp.StatusCode, sawConditional.Load())
	}
}
