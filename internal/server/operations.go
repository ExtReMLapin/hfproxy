package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
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
	release := func() { s.stagingReserved.Add(-size) }
	if free, ok := diskFree(s.stagingDirectory()); ok && uint64(reserved) > free {
		release()
		return nil, errStagingFull
	}
	return release, nil
}

func (s *Server) startMaintenance(ctx context.Context) {
	go func() {
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		for tick := 0; ; tick++ {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			if evicted, err := s.evictOverQuota(ctx); err != nil {
				if ctx.Err() == nil {
					s.logger.Warn("cache eviction failed", "class", fmt.Sprintf("%T", err))
				}
			} else if evicted > 0 {
				s.logger.Info("cache eviction completed", "artifacts", evicted)
			}
			if tick%6 == 0 {
				s.runRetention(ctx)
			}
			timer.Reset(maintenanceInterval)
		}
	}()
}

// runRetention removes expired sessions and, when configured, old download
// events and audit events, in bounded batches.
func (s *Server) runRetention(ctx context.Context) {
	cfg := s.currentConfig()
	jobs := []struct {
		table string
		query string
		args  []any
	}{
		{"sessions", `DELETE FROM sessions WHERE token_hash IN (SELECT token_hash FROM sessions WHERE expires_at < now() LIMIT $1)`, nil},
		{"login_attempts", `DELETE FROM login_attempts WHERE key IN (SELECT key FROM login_attempts WHERE window_start < now()-interval '1 hour' LIMIT $1)`, nil},
	}
	if cfg.RetentionDownloads > 0 {
		jobs = append(jobs, struct {
			table string
			query string
			args  []any
		}{"downloads", `DELETE FROM downloads WHERE id IN (SELECT id FROM downloads WHERE started_at < now()-make_interval(secs => $2::double precision) LIMIT $1)`, []any{cfg.RetentionDownloads.Seconds()}})
	}
	if cfg.RetentionAudit > 0 {
		jobs = append(jobs, struct {
			table string
			query string
			args  []any
		}{"audit_events", `DELETE FROM audit_events WHERE id IN (SELECT id FROM audit_events WHERE created_at < now()-make_interval(secs => $2::double precision) LIMIT $1)`, []any{cfg.RetentionAudit.Seconds()}})
	}
	for _, job := range jobs {
		deleted, err := s.deleteInBatches(ctx, job.query, job.args)
		if deleted > 0 {
			if s.metrics != nil {
				s.metrics.add("hfproxy_retention_deleted_total", []string{"table=" + job.table}, float64(deleted))
			}
			s.logger.Info("retention purge completed", "table", job.table, "deleted", deleted)
		}
		if err != nil {
			if ctx.Err() == nil {
				s.logger.Warn("retention purge failed", "table", job.table, "class", fmt.Sprintf("%T", err))
			}
			continue
		}
	}
}

func (s *Server) deleteInBatches(ctx context.Context, query string, extra []any) (int64, error) {
	var total int64
	args := append([]any{retentionBatch}, extra...)
	for i := 0; i < retentionMaxBatches; i++ {
		result, err := s.db.SQL.ExecContext(ctx, query, args...)
		if err != nil {
			return total, err
		}
		n, _ := result.RowsAffected()
		total += n
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
	var total int64
	if err := s.db.SQL.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes),0) FROM blobs WHERE orphaned_at IS NULL`).Scan(&total); err != nil {
		return 0, err
	}
	if total <= limit {
		return 0, nil
	}
	target := int64(float64(limit) * evictionTargetPercent / 100)
	evicted := 0
	for total > target {
		rows, err := s.db.SQL.QueryContext(ctx, `SELECT id,blob_id FROM artifacts ORDER BY last_accessed_at ASC, id LIMIT $1`, evictionBatch)
		if err != nil {
			return evicted, err
		}
		type candidate struct{ id, blobID string }
		var batch []candidate
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.id, &c.blobID); err != nil {
				rows.Close()
				return evicted, err
			}
			batch = append(batch, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return evicted, err
		}
		if len(batch) == 0 {
			break
		}
		for _, c := range batch {
			freed, err := s.evictArtifact(ctx, c.id, c.blobID)
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
	return evicted, nil
}

func (s *Server) evictArtifact(ctx context.Context, artifactID, blobID string) (int64, error) {
	tx, err := s.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Lock the blob first, the same order publication uses, so a concurrent
	// fill cannot attach a new artifact between the count and the orphan mark.
	var size int64
	if err := tx.QueryRowContext(ctx, `SELECT size_bytes FROM blobs WHERE id=$1 FOR UPDATE`, blobID).Scan(&size); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE id=$1`, artifactID); err != nil {
		return 0, err
	}
	var refs int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE blob_id=$1`, blobID).Scan(&refs); err != nil {
		return 0, err
	}
	var freed int64
	if refs == 0 {
		result, err := tx.ExecContext(ctx, `UPDATE blobs SET orphaned_at=now() WHERE id=$1 AND orphaned_at IS NULL`, blobID)
		if err != nil {
			return 0, err
		}
		if n, _ := result.RowsAffected(); n > 0 {
			freed = size
		}
	}
	return freed, tx.Commit()
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
