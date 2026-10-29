package server

import (
	"crypto/sha256"
	"sync"
	"time"
)

// authCache remembers who a credential belongs to for a few seconds, so a burst
// of requests with the same token or session costs one database lookup instead
// of one (or several) each. It only holds successful lookups, never a plaintext
// credential (entries are keyed by its SHA-256), and is emptied whenever this
// instance revokes a token, ends a session or changes a user. Changes made by
// another replica or the CLI are seen after at most the TTL.
type authCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[[32]byte]authCacheEntry
	// generation counts purges: a lookup that began before a purge must not put
	// its answer back afterwards, or it would outlive the revocation.
	generation uint64
}

type authCacheEntry struct {
	principal Principal
	expires   time.Time
}

const authCacheMaxEntries = 4096

func newAuthCache(ttl time.Duration) *authCache {
	return &authCache{ttl: ttl, max: authCacheMaxEntries, entries: map[[32]byte]authCacheEntry{}}
}

func authCacheKey(kind, credential string) [32]byte {
	return sha256.Sum256([]byte(kind + "\x00" + credential))
}

func (c *authCache) get(key [32]byte) (*Principal, bool) {
	if c == nil || c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expires) {
		delete(c.entries, key)
		return nil, false
	}
	p := entry.principal
	p.Scopes = append([]string(nil), entry.principal.Scopes...)
	return &p, true
}

// currentGeneration is read before a database lookup and passed to put.
func (c *authCache) currentGeneration() uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

// put remembers a lookup made while the cache was at generation gen; it is
// ignored when a purge has happened since.
func (c *authCache) put(key [32]byte, p *Principal, gen uint64) {
	if c == nil || c.ttl <= 0 || p == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.generation {
		return
	}
	now := time.Now()
	if len(c.entries) >= c.max {
		for k, e := range c.entries {
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= c.max {
			c.entries = map[[32]byte]authCacheEntry{}
		}
	}
	stored := *p
	stored.Scopes = append([]string(nil), p.Scopes...)
	c.entries[key] = authCacheEntry{principal: stored, expires: now.Add(c.ttl)}
}

// purge forgets every entry. Call it after any change that must take effect
// at once on this instance.
func (c *authCache) purge() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries = map[[32]byte]authCacheEntry{}
	c.generation++
	c.mu.Unlock()
}
