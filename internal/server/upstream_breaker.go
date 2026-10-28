package server

import (
	"errors"
	"sync"
	"time"
)

// errUpstreamCircuitOpen is returned without contacting the Hub while a host's
// circuit is open.
var errUpstreamCircuitOpen = errors.New("upstream circuit open")

// circuitBreaker stops requests to a host that keeps failing, so each caller
// does not wait through timeouts and retries only to fail, and the host is not
// hammered while it recovers.
//
// Closed: requests flow and consecutive failures are counted. After threshold
// failures it opens for the cooldown, and requests are refused at once. When the
// cooldown passes, exactly one request is let through as a probe: if it succeeds
// the circuit closes, if it fails the circuit opens again for another cooldown.
type circuitBreaker struct {
	mu        sync.Mutex
	failures  int
	openUntil time.Time
	probing   bool
}

// Allow reports whether a request may be sent. A threshold of zero disables the
// breaker.
func (b *circuitBreaker) Allow(now time.Time, threshold int) bool {
	if threshold <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true
	}
	if now.Before(b.openUntil) || b.probing {
		return false
	}
	b.probing = true
	return true
}

// Success closes the circuit.
func (b *circuitBreaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures, b.openUntil, b.probing = 0, time.Time{}, false
}

// Failure records a failed request and reports whether the circuit just opened
// (or reopened after a failed probe).
func (b *circuitBreaker) Failure(now time.Time, threshold int, cooldown time.Duration) bool {
	if threshold <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.probing {
		b.probing = false
		b.openUntil = now.Add(cooldown)
		return true
	}
	b.failures++
	if b.failures >= threshold && b.openUntil.IsZero() {
		b.openUntil = now.Add(cooldown)
		return true
	}
	return false
}

// Abort releases a probe whose request ended without a verdict, for example
// because the client went away.
func (b *circuitBreaker) Abort() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
}

func (s *Server) upstreamBreaker(host string) *circuitBreaker {
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()
	if s.breakers == nil {
		s.breakers = make(map[string]*circuitBreaker)
	}
	breaker := s.breakers[host]
	if breaker == nil {
		breaker = &circuitBreaker{}
		s.breakers[host] = breaker
	}
	return breaker
}
