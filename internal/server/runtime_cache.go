package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/hfproxy/hfproxy/internal/storage"
)

const negativeCacheMaxEntries = 10_000

func (s *Server) negativeLookup(key string) (int, http.Header, bool) {
	s.negativeMu.Lock()
	defer s.negativeMu.Unlock()
	entry, ok := s.negative[key]
	if !ok || time.Now().After(entry.expires) {
		if ok {
			delete(s.negative, key)
		}
		return 0, nil, false
	}
	return entry.status, entry.headers.Clone(), true
}

func (s *Server) negativeStore(key string, status int, headers http.Header) {
	s.negativeMu.Lock()
	defer s.negativeMu.Unlock()
	if s.negative == nil {
		s.negative = make(map[string]negativeCacheEntry)
	}
	now := time.Now()
	for existing, entry := range s.negative {
		if !now.Before(entry.expires) {
			delete(s.negative, existing)
		}
	}
	if _, exists := s.negative[key]; !exists && len(s.negative) >= negativeCacheMaxEntries {
		oldestKey := ""
		var oldestExpiry time.Time
		for existing, entry := range s.negative {
			if oldestKey == "" || entry.expires.Before(oldestExpiry) {
				oldestKey, oldestExpiry = existing, entry.expires
			}
		}
		delete(s.negative, oldestKey)
	}
	selected := make(http.Header)
	for _, name := range []string{"X-Error-Code", "X-Error-Message", "X-Repo-Commit", "Retry-After"} {
		if value := headers.Get(name); value != "" {
			selected.Set(name, value)
		}
	}
	s.negative[key] = negativeCacheEntry{expires: now.Add(s.currentConfig().CacheNegativeTTL), status: status, headers: selected}
}

func (s *Server) joinCacheMiss(key string) (bool, <-chan struct{}) {
	s.missMu.Lock()
	defer s.missMu.Unlock()
	if s.misses == nil {
		s.misses = make(map[string]*cacheMiss)
	}
	if flight, ok := s.misses[key]; ok {
		return false, flight.done
	}
	s.misses[key] = &cacheMiss{done: make(chan struct{})}
	return true, s.misses[key].done
}

func (s *Server) finishCacheMiss(key string) {
	s.missMu.Lock()
	defer s.missMu.Unlock()
	if flight, ok := s.misses[key]; ok {
		delete(s.misses, key)
		close(flight.done)
	}
}

func (s *Server) startOrphanedBlobGC(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.runBlobGC(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
}

// runBlobGC runs one collection and reports its outcome in logs and metrics.
func (s *Server) runBlobGC(ctx context.Context) {
	started := time.Now()
	deleted, err := s.collectOrphanedBlobsCounted(ctx)
	result := "success"
	if err != nil {
		result = "error"
		if ctx.Err() == nil {
			s.logger.Warn("orphaned blob collection failed", "class", fmt.Sprintf("%T", err), "deleted", deleted)
		}
	} else if deleted > 0 {
		s.logger.Info("orphaned blob collection completed", "deleted", deleted, "duration_ms", time.Since(started).Milliseconds())
	}
	if s.metrics != nil {
		s.metrics.add("hfproxy_gc_runs_total", []string{"result=" + result}, 1)
		s.metrics.add("hfproxy_gc_blobs_deleted_total", nil, float64(deleted))
		s.metrics.setGauge("hfproxy_gc_last_run_timestamp_seconds", float64(time.Now().Unix()))
	}
}

func (s *Server) collectOrphanedBlobs(ctx context.Context) error {
	_, err := s.collectOrphanedBlobsCounted(ctx)
	return err
}

// collectOrphanedBlobsCounted deletes every orphaned blob past the grace
// period. A blob that fails does not stop the others; the first error is
// returned after the pass.
func (s *Server) collectOrphanedBlobsCounted(ctx context.Context) (int, error) {
	objects := s.currentStorage()
	if objects == nil || s.currentConfig().CacheGCGrace < 0 {
		return 0, nil
	}
	ids, err := s.db.OrphanedBlobIDs(ctx, s.currentConfig().CacheGCGrace)
	if err != nil {
		return 0, err
	}
	deleted := 0
	var firstErr error
	for _, id := range ids {
		if ctx.Err() != nil {
			return deleted, ctx.Err()
		}
		removed, err := s.collectOrphanedBlob(ctx, objects, id)
		if removed {
			deleted++
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return deleted, firstErr
}

// collectOrphanedBlob removes one blob and its object if it is still orphaned
// and unreferenced once locked. It reports whether the blob was deleted.
func (s *Server) collectOrphanedBlob(ctx context.Context, objects *storage.S3, id string) (bool, error) {
	return s.db.CollectOrphanedBlob(ctx, id, s.currentConfig().CacheGCGrace, objects.Delete)
}
