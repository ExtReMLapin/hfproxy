package server

import (
	"container/list"
	"net/http"
	"sync"
	"time"
)

// apiCacheEntry is a successful Hub metadata response kept in memory. The
// stored headers are the Hub's own, so Link rewriting and the like happen when
// the entry is served, for whichever client asks.
type apiCacheEntry struct {
	key      string
	header   http.Header
	body     []byte
	etag     string
	storedAt time.Time
}

func (e *apiCacheEntry) size() int64 { return int64(len(e.body) + len(e.key) + 512) }

// apiCache is a size-bounded least-recently-used cache of Hub metadata
// responses. It is per process and holds nothing a client could not already
// read: access to the repository is checked before it is consulted.
type apiCache struct {
	mu       sync.Mutex
	maxBytes int64
	bytes    int64
	order    *list.List // front is the most recently used
	items    map[string]*list.Element
}

func newAPICache(maxBytes int64) *apiCache {
	return &apiCache{maxBytes: maxBytes, order: list.New(), items: make(map[string]*list.Element)}
}

func (c *apiCache) get(key string) (apiCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.items[key]
	if !ok {
		return apiCacheEntry{}, false
	}
	c.order.MoveToFront(element)
	entry := *element.Value.(*apiCacheEntry)
	return entry, true
}

// put stores an entry, evicting the least recently used ones to stay within
// the limit. An entry larger than the whole cache is not stored.
func (c *apiCache) put(entry apiCacheEntry) {
	if entry.size() > c.maxBytes {
		c.delete(entry.key)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[entry.key]; ok {
		c.bytes -= element.Value.(*apiCacheEntry).size()
		c.order.Remove(element)
		delete(c.items, entry.key)
	}
	stored := entry
	c.items[entry.key] = c.order.PushFront(&stored)
	c.bytes += stored.size()
	for c.bytes > c.maxBytes {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		evicted := oldest.Value.(*apiCacheEntry)
		c.bytes -= evicted.size()
		c.order.Remove(oldest)
		delete(c.items, evicted.key)
	}
}

// refresh marks an entry as just confirmed by the Hub.
func (c *apiCache) refresh(key string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[key]; ok {
		element.Value.(*apiCacheEntry).storedAt = at
	}
}

func (c *apiCache) delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[key]; ok {
		c.bytes -= element.Value.(*apiCacheEntry).size()
		c.order.Remove(element)
		delete(c.items, key)
	}
}

func (c *apiCache) usage() (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items), c.bytes
}

// apiFlight lets concurrent requests for the same uncached key share one Hub
// request.
type apiFlight struct{ done chan struct{} }

func (s *Server) metadataCache() *apiCache {
	s.apiCacheOnce.Do(func() {
		if limit := s.currentConfig().APICacheMaxBytes; limit > 0 {
			s.apiCache = newAPICache(limit)
		}
	})
	return s.apiCache
}

func (s *Server) joinAPIFlight(key string) (*apiFlight, bool) {
	s.apiFlightMu.Lock()
	defer s.apiFlightMu.Unlock()
	if s.apiFlights == nil {
		s.apiFlights = make(map[string]*apiFlight)
	}
	if flight, ok := s.apiFlights[key]; ok {
		return flight, false
	}
	flight := &apiFlight{done: make(chan struct{})}
	s.apiFlights[key] = flight
	return flight, true
}

func (s *Server) finishAPIFlight(key string, flight *apiFlight) {
	s.apiFlightMu.Lock()
	delete(s.apiFlights, key)
	s.apiFlightMu.Unlock()
	close(flight.done)
}
