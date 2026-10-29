package server

import (
	"context"
	"sync"
	"time"

	"github.com/hfproxy/hfproxy/internal/db"
)

// statsCacheTTL is how long the dashboard figures are reused. The console asks
// every time a page is opened, and the figures need several queries; hits and
// misses come from per-repository counters, not from the downloads table.
const statsCacheTTL = 15 * time.Second

// statsCache keeps the figures for a short time per viewer. Concurrent requests
// for the same viewer wait for one computation instead of each running the
// query. A zero ttl turns it off (servers built by tests).
type statsCache struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]*statsEntry
}

type statsEntry struct {
	mu      sync.Mutex
	stats   db.Stats
	expires time.Time
}

func (c *statsCache) get(ctx context.Context, role, userID string, compute func(context.Context) (db.Stats, error)) (db.Stats, error) {
	if c.ttl <= 0 {
		return compute(ctx)
	}
	// An administrator sees everything, so all administrators share one entry.
	key := "admin"
	if role != "admin" {
		key = role + ":" + userID
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]*statsEntry)
	}
	if len(c.entries) > 512 {
		now := time.Now()
		for k, e := range c.entries {
			if k != key && now.After(e.expires) {
				delete(c.entries, k)
			}
		}
	}
	entry := c.entries[key]
	if entry == nil {
		entry = &statsEntry{}
		c.entries[key] = entry
	}
	c.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if time.Now().Before(entry.expires) {
		return entry.stats, nil
	}
	stats, err := compute(ctx)
	if err != nil {
		return stats, err
	}
	entry.stats, entry.expires = stats, time.Now().Add(c.ttl)
	return stats, nil
}
