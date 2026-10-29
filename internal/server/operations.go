package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hfproxy/hfproxy/internal/db"
)

var (
	errFillBusy    = errors.New("cache fill capacity exhausted")
	errStagingFull = errors.New("insufficient staging space")

	// fillSlotWait is how long a cache fill waits for a free slot before the
	// client is told to retry.
	fillSlotWait = 15 * time.Second
)

const (
	defaultFillConcurrency = 4
	maintenanceInterval    = 10 * time.Minute
	retentionBatch         = 5000
	retentionMaxBatches    = 200
	evictionBatch          = 200
	evictionTargetPercent  = 90
)

// acquireFillSlot bounds how many cache fills (and their staging files) run at
// once. A Server without slots, such as one built by tests, is unbounded.
func (s *Server) acquireFillSlot(ctx context.Context) (func(), error) {
	if s.fillSlots == nil {
		return func() {}, nil
	}
	timer := time.NewTimer(fillSlotWait)
	defer timer.Stop()
	select {
	case s.fillSlots <- struct{}{}:
		return func() { <-s.fillSlots }, nil
	case <-timer.C:
		return nil, errFillBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) stagingDirectory() string {
	if dir := s.currentConfig().StagingDir; dir != "" {
		return dir
	}
	return os.TempDir()
}

func (s *Server) createStagingFile(pattern string) (*os.File, error) {
	return os.CreateTemp(s.currentConfig().StagingDir, pattern)
}

// reserveStaging refuses a fill whose file would not fit next to the fills
// already running. The reservation counts the full size of every running fill
// even though part of it is already on disk, which errs on the safe side.
func (s *Server) reserveStaging(size int64) (func(), error) {
	reserved := s.stagingReserved.Add(size)
	release := func() {
		s.observeStaging(s.stagingReserved.Add(-size))
	}
	if free, ok := diskFree(s.stagingDirectory()); ok && uint64(reserved) > free {
		release()
		return nil, errStagingFull
	}
	s.observeStaging(reserved)
	return release, nil
}

// observeStaging publishes how much staging space is free and how much running
// fills have reserved.
func (s *Server) observeStaging(reserved int64) {
	if s.metrics == nil {
		return
	}
	s.metrics.setGauge("hfproxy_staging_reserved_bytes", float64(reserved))
	if free, ok := diskFree(s.stagingDirectory()); ok {
		s.metrics.setGauge("hfproxy_staging_free_bytes", float64(free))
	}
}

// stagingLow reports whether the staging directory has less free space than
// HFPROXY_STAGING_MIN_FREE_BYTES. It is false when the check is off or the free
// space cannot be read.
func (s *Server) stagingLow() bool {
	minimum := s.currentConfig().StagingMinFreeBytes
	if minimum <= 0 {
		return false
	}
	free, ok := diskFree(s.stagingDirectory())
	return ok && free < uint64(minimum)
}

func (s *Server) startMaintenance(ctx context.Context) {
	go func() {
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		for tick := 0; ; {
			evictionOnly := false
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			case <-s.maintenanceWake:
				evictionOnly = true
			}
			s.observeStaging(s.stagingReserved.Load())
			if evicted, err := s.evictOverQuota(ctx); err != nil {
				if ctx.Err() == nil {
					s.logger.Warn("cache eviction failed", "class", fmt.Sprintf("%T", err))
				}
			} else if evicted > 0 {
				s.logger.Info("cache eviction completed", "artifacts", evicted)
			}
			if evictionOnly {
				continue // the periodic timer is still running
			}
			if tick%6 == 0 {
				s.runRetention(ctx)
			}
			tick++
			timer.Reset(maintenanceInterval)
		}
	}()
}

// runRetention removes expired sessions and, when configured, old download
// events and audit events, in bounded batches.
func (s *Server) runRetention(ctx context.Context) {
	cfg := s.currentConfig()
	type job struct {
		name      db.RetentionJob
		olderThan time.Duration
	}
	jobs := []job{{name: db.RetainSessions}, {name: db.RetainAbandonedDownloads}, {name: db.RetainLoginAttempts}}
	if cfg.RetentionDownloads > 0 {
		jobs = append(jobs, job{db.RetainDownloads, cfg.RetentionDownloads})
	}
	if cfg.RetentionAudit > 0 {
		jobs = append(jobs, job{db.RetainAudit, cfg.RetentionAudit})
	}
	for _, j := range jobs {
		deleted, err := s.retainInBatches(ctx, j.name, j.olderThan)
		if deleted > 0 {
			if s.metrics != nil {
				s.metrics.add("hfproxy_retention_deleted_total", []string{"table=" + string(j.name)}, float64(deleted))
			}
			s.logger.Info("retention purge completed", "table", string(j.name), "deleted", deleted)
		}
		if err != nil && ctx.Err() == nil {
			s.logger.Warn("retention purge failed", "table", string(j.name), "class", fmt.Sprintf("%T", err))
		}
	}
}

func (s *Server) retainInBatches(ctx context.Context, job db.RetentionJob, olderThan time.Duration) (int64, error) {
	var total int64
	for i := 0; i < retentionMaxBatches; i++ {
		n, err := s.db.RunRetention(ctx, job, retentionBatch, olderThan)
		total += n
		if err != nil {
			return total, err
		}
		if n < retentionBatch {
			return total, nil
		}
	}
	return total, nil
}

// evictOverQuota enforces CacheMaxTotalSize: when live blobs exceed the quota
// it drops the least recently used artifacts until usage is back under 90% of
// it. Blobs left without artifacts are marked orphaned, and the existing
// garbage collector deletes their objects after the grace period.
func (s *Server) evictOverQuota(ctx context.Context) (int, error) {
	limit := s.currentConfig().CacheMaxTotalSize
	if limit <= 0 {
		return 0, nil
	}
	total, err := s.db.LiveBytes(ctx)
	if err != nil {
		return 0, err
	}
	if total <= limit {
		return 0, nil
	}
	target := int64(float64(limit) * evictionTargetPercent / 100)
	evicted := 0
	for total > target {
		batch, err := s.db.EvictionCandidates(ctx, evictionBatch)
		if err != nil {
			return evicted, err
		}
		if len(batch) == 0 {
			break
		}
		for _, c := range batch {
			freed, err := s.db.EvictArtifact(ctx, c.ArtifactID, c.BlobID)
			if err != nil {
				return evicted, err
			}
			total -= freed
			evicted++
			if s.metrics != nil {
				s.metrics.add("hfproxy_cache_evictions_total", nil, 1)
			}
			if total <= target {
				break
			}
		}
	}
	if total > limit {
		s.logger.Warn("the cache is over its size limit but what remains is pinned or still referenced", "limit_bytes", limit, "used_bytes", total)
	}
	return evicted, nil
}

// pinnedBytes is the size of the live blobs that eviction must not touch.
func (s *Server) pinnedBytes(ctx context.Context) (int64, error) {
	return s.db.PinnedBytes(ctx)
}

var (
	// apiReadTimeout bounds how long a client may take to send an API or
	// login request, body included; apiWriteTimeout bounds the response.
	apiReadTimeout  = 30 * time.Second
	apiWriteTimeout = 5 * time.Minute
)

// requestDeadlines gives every request except file downloads a read and write
// deadline, so a client dribbling a request body cannot hold a connection.
// Downloads stream for as long as the transfer makes progress and are bound by
// the idle timeouts on the upstream and storage sides instead.
func (s *Server) requestDeadlines(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/resolve/") {
			controller := http.NewResponseController(w)
			_ = controller.SetReadDeadline(time.Now().Add(apiReadTimeout))
			_ = controller.SetWriteDeadline(time.Now().Add(apiWriteTimeout))
		}
		next.ServeHTTP(w, r)
	})
}

// metricsHandler serves the Prometheus endpoint on the dedicated listener.
func (s *Server) metricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.currentConfig()
		path := cfg.MetricsPath
		if path == "" {
			path = "/metrics"
		}
		if !cfg.MetricsEnabled || r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		s.metrics.handler().ServeHTTP(w, r)
	})
}

// metricsOnMainListener reports whether /metrics is served on the public port.
func metricsOnMainListener(addr string) bool { return addr == "" }
