package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/hfproxy/hfproxy/internal/bytesize"
)

// Download limits keep one client from taking all the bandwidth or storage
// traffic of the service. They apply to downloads of repository files made by
// users, tokens and anonymous clients; preloads started by an administrator
// are exempt.
//
//   - concurrent transfers per user (or per client address when anonymous),
//     counted on this instance;
//   - a speed cap per transfer;
//   - a daily quota per user, counted over the last 24 hours from the download
//     history, so it is shared by all instances. It is checked when a transfer
//     starts: a transfer already running is never cut.

type downloadAdmittedKey struct{}

// usageCacheTTL is how long a user's usage figures are reused, so a burst of
// requests does not query the download history for each of them.
const usageCacheTTL = 10 * time.Second

type usageEntry struct {
	quota   *int64
	used    int64
	fetched time.Time
}

type downloadLimiter struct {
	mu     sync.Mutex
	active map[string]int
	usage  map[string]usageEntry
}

// admitDownload applies the limits to a file request. It returns the request to
// use from then on (marked as admitted, so a request that re-enters resolveFile
// is not counted twice), a function releasing the slot, and false after
// answering the client when the request is refused.
func (s *Server) admitDownload(w http.ResponseWriter, r *http.Request, p *Principal) (*http.Request, http.ResponseWriter, func(), bool) {
	nothing := func() {}
	cfg := s.currentConfig()
	if r.Method != http.MethodGet || isPreloadFetch(r.Context()) || r.Context().Value(downloadAdmittedKey{}) != nil {
		return r, w, nothing, true
	}
	r = r.WithContext(context.WithValue(r.Context(), downloadAdmittedKey{}, true))
	if cfg.DownloadMaxConcurrent == 0 && cfg.DownloadRateLimit == 0 && cfg.DownloadDailyQuota == 0 && !s.anyUserQuota() {
		return r, w, nothing, true
	}
	key := "ip:" + s.loginClientIP(r)
	userID := ""
	if p != nil {
		key, userID = "user:"+p.ID, p.ID
	}

	if userID != "" {
		if refused, limit, used := s.overQuota(r.Context(), userID, cfg.DownloadDailyQuota); refused {
			s.rejectDownload(w, r, "quota", userID, limit, used, 600)
			return r, w, nothing, false
		}
	}

	release := nothing
	if max := cfg.DownloadMaxConcurrent; max > 0 {
		l := s.downloadLimits()
		l.mu.Lock()
		// The count is read while the lock is held: another request giving its
		// slot back changes the map under the same lock, and reading a map while
		// it is written is a fatal error that takes the whole instance down.
		running := int64(l.active[key])
		if running >= max {
			l.mu.Unlock()
			s.rejectDownload(w, r, "concurrency", userID, max, running, 5)
			return r, w, nothing, false
		}
		l.active[key]++
		l.mu.Unlock()
		var once sync.Once
		release = func() {
			once.Do(func() {
				l.mu.Lock()
				defer l.mu.Unlock()
				if l.active[key] <= 1 {
					delete(l.active, key)
				} else {
					l.active[key]--
				}
			})
		}
	}
	if bps := cfg.DownloadRateLimit; bps > 0 {
		w = newThrottledWriter(r.Context(), w, bps)
	}
	if cfg.DownloadMaxConcurrent > 0 {
		// A cache fill goes on publishing after the last byte has reached the
		// client; the next request on the same connection must not be refused
		// for that, so the slot is given back as soon as the body is complete.
		w = &slotWriter{ResponseWriter: w, release: release, length: -1}
	}
	s.logger.Debug("download admitted", "request_id", requestID(r.Context()), "user_id", userID, "max_concurrent", cfg.DownloadMaxConcurrent, "bytes_per_second", cfg.DownloadRateLimit)
	return r, w, release, true
}

// anyUserQuota is true when some user has a quota of their own. Quotas set on
// users apply even when no default is configured, so the check cannot be
// skipped just because the default is off; the answer is remembered briefly.
func (s *Server) anyUserQuota() bool {
	l := s.downloadLimits()
	l.mu.Lock()
	entry, ok := l.usage["*any"]
	l.mu.Unlock()
	if ok && time.Since(entry.fetched) < usageCacheTTL {
		return entry.used > 0
	}
	// The query runs outside the lock: under it, every admission would wait
	// for the database each time the answer expires.
	exists, err := s.db.AnyUserDailyQuota(context.Background())
	if err != nil {
		return false
	}
	used := int64(0)
	if exists {
		used = 1
	}
	l.mu.Lock()
	l.usage["*any"] = usageEntry{used: used, fetched: time.Now()}
	l.mu.Unlock()
	return exists
}

// overQuota reports whether the user has used up their daily quota, with the
// limit that applied and the bytes used.
func (s *Server) overQuota(ctx context.Context, userID string, defaultQuota int64) (bool, int64, int64) {
	l := s.downloadLimits()
	l.mu.Lock()
	entry, ok := l.usage[userID]
	l.mu.Unlock()
	if !ok || time.Since(entry.fetched) >= usageCacheTTL {
		quota, used, err := s.db.DownloadUsage(ctx, userID)
		if err != nil {
			// A failure to read the history must not stop downloads.
			s.logger.Warn("download quota could not be checked", "request_id", requestID(ctx), "user_id", userID, "error", safeLogValue(err.Error(), 200))
			return false, 0, 0
		}
		entry = usageEntry{quota: quota, used: used, fetched: time.Now()}
		l.mu.Lock()
		if len(l.usage) > 10000 {
			l.usage = map[string]usageEntry{}
		}
		l.usage[userID] = entry
		l.mu.Unlock()
	}
	limit := defaultQuota
	if entry.quota != nil {
		limit = *entry.quota
	}
	return limit > 0 && entry.used >= limit, limit, entry.used
}

func (s *Server) rejectDownload(w http.ResponseWriter, r *http.Request, reason, userID string, limit, used int64, retryAfter int) {
	if s.metrics != nil {
		s.metrics.add("hfproxy_download_rejected_total", []string{"reason=" + reason}, 1)
	}
	s.logger.Warn("download refused: limit reached", "request_id", requestID(r.Context()), "user_id", userID, "client_ip", s.loginClientIP(r), "reason", reason, "limit", limit, "current", used)
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	if reason == "quota" {
		writeProblem(w, r, http.StatusTooManyRequests, "Daily download quota reached", fmt.Sprintf("This account has downloaded its daily allowance (%s). Try again later or ask an administrator.", bytesize.Format(limit)))
		return
	}
	writeProblem(w, r, http.StatusTooManyRequests, "Too many downloads at once", fmt.Sprintf("At most %d downloads can run at the same time for one account. Wait for one to finish.", limit))
}

// forget drops what is remembered about a user's usage, after their quota changed.
func (l *downloadLimiter) forget(userID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.usage, userID)
	delete(l.usage, "*any")
}

func (s *Server) downloadLimits() *downloadLimiter {
	s.downloadLimitsOnce.Do(func() {
		s.downloadLimiterStore = &downloadLimiter{active: map[string]int{}, usage: map[string]usageEntry{}}
	})
	return s.downloadLimiterStore
}

// throttledWriter slows a response to a number of bytes per second. It lets
// about a quarter of a second of data through at once, then paces the rest.
type throttledWriter struct {
	http.ResponseWriter
	ctx   context.Context
	bps   int64
	chunk int
	next  time.Time // when the bytes sent so far would be due at the limit
}

const throttleBurst = 250 * time.Millisecond

func newThrottledWriter(ctx context.Context, w http.ResponseWriter, bps int64) *throttledWriter {
	chunk := int(bps / 8)
	if chunk < 4096 {
		chunk = 4096
	}
	if chunk > 256<<10 {
		chunk = 256 << 10
	}
	return &throttledWriter{ResponseWriter: w, ctx: ctx, bps: bps, chunk: chunk}
}

func (t *throttledWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > t.chunk {
			n = t.chunk
		}
		now := time.Now()
		if t.next.Before(now) {
			t.next = now
		}
		if wait := time.Until(t.next) - throttleBurst; wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-t.ctx.Done():
				timer.Stop()
				return written, t.ctx.Err()
			}
		}
		m, err := t.ResponseWriter.Write(p[:n])
		written += m
		t.next = t.next.Add(time.Duration(float64(m) / float64(t.bps) * float64(time.Second)))
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

func (t *throttledWriter) Flush() {
	if flusher, ok := t.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (t *throttledWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// slotWriter gives a download slot back as soon as the whole body has been
// written, instead of when the handler finishes.
type slotWriter struct {
	http.ResponseWriter
	release func()
	length  int64 // -1 until the first write
	written int64
}

func (s *slotWriter) Write(p []byte) (int, error) {
	if s.length < 0 {
		s.length = 0
		if value, err := strconv.ParseInt(s.Header().Get("Content-Length"), 10, 64); err == nil && value > 0 {
			s.length = value
		}
	}
	n, err := s.ResponseWriter.Write(p)
	s.written += int64(n)
	if s.length > 0 && s.written >= s.length {
		s.release()
	}
	return n, err
}

func (s *slotWriter) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *slotWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
