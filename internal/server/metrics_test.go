package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMetricsMiddlewareCountsNormalizedRouteAndStreams(t *testing.T) {
	m := newMetrics()
	h := m.middleware("/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.cacheDecision(r.Context(), "miss")
		m.cacheDecision(r.Context(), "hit")
		http.ServeContent(w, r, "weights.bin", time.Time{}, bytes.NewReader([]byte("stream")))
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/models/private/repo/resolve/main/weights.bin?token=secret", nil)
	req.Header.Set("Range", "bytes=1-3")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d", rec.Code)
	}
	out := httptest.NewRecorder()
	m.handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := out.Body.String()
	if !strings.Contains(body, `hfproxy_http_requests_total{method="GET",route="/models/{repo}/resolve/{revision}/{file}",status_code="206"} 1`) {
		t.Fatalf("missing normalized request metric:\n%s", body)
	}
	if rec.Body.String() != "tre" {
		t.Fatalf("range response = %q", rec.Body.String())
	}
	if !strings.Contains(body, `hfproxy_http_response_bytes_total{route="/models/{repo}/resolve/{revision}/{file}"} 3`) {
		t.Fatalf("missing streamed byte count:\n%s", body)
	}
	if !strings.Contains(body, `hfproxy_cache_requests_total{result="miss"} 1`) || strings.Contains(body, `hfproxy_cache_requests_total{result="hit"} 1`) {
		t.Fatalf("cache decision was double counted:\n%s", body)
	}
	if !strings.Contains(body, `hfproxy_downloads_total{result="partial"} 1`) {
		t.Fatalf("partial download session missing:\n%s", body)
	}
	if strings.Contains(body, "private") || strings.Contains(body, "weights.bin") || strings.Contains(body, "secret") {
		t.Fatal("sensitive URL data leaked into metrics")
	}
}

func TestCancelledDownloadClosesInFlightGauge(t *testing.T) {
	m := newMetrics()
	ctx, cancel := context.WithCancel(context.Background())
	h := m.middleware("/metrics", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { cancel(); w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodGet, "/models/org/repo/resolve/main/file.bin", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := httptest.NewRecorder()
	m.handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := out.Body.String()
	if !strings.Contains(body, `hfproxy_downloads_total{result="cancelled"} 1`) || !strings.Contains(body, "hfproxy_downloads_in_flight 0") {
		t.Fatalf("cancelled session/gauge =\n%s", body)
	}
}

func TestMetricsEndpointIsExcludedFromRequestMetrics(t *testing.T) {
	m := newMetrics()
	h := m.middleware("/metrics", m.handler())
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "hfproxy_http_requests_total" {
			t.Fatal("metrics scrape was recorded as an application request")
		}
	}
}

func TestMetricsConcurrentRequests(t *testing.T) {
	m := newMetrics()
	h := m.middleware("/metrics", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	const count = 64
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
		}()
	}
	wg.Wait()
	out := httptest.NewRecorder()
	m.handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(out.Body.String(), `hfproxy_http_requests_total{method="GET",route="/healthz",status_code="200"} 64`) {
		t.Fatalf("concurrent requests were lost:\n%s", out.Body.String())
	}
}

func TestDownloadTimeToFirstByteIsSplitByCacheResult(t *testing.T) {
	m := newMetrics()
	serve := func(path, cache string, status int, delay time.Duration) {
		h := m.middleware("/metrics", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(delay)
			if cache != "" {
				w.Header().Set("X-Cache", cache)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte("body"))
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	serve("/models/org/repo/resolve/main/a.bin", "HIT", http.StatusOK, 0)
	serve("/models/org/repo/resolve/main/b.bin", "HIT", http.StatusOK, 0)
	serve("/models/org/repo/resolve/main/c.bin", "MISS", http.StatusOK, 30*time.Millisecond)
	serve("/models/org/repo/resolve/main/d.bin", "STALE", http.StatusOK, 0)
	serve("/models/org/repo/resolve/main/e.bin", "", http.StatusOK, 0)
	serve("/models/org/repo/resolve/main/f.bin", "", http.StatusNotFound, 0) // an error is not a latency sample
	serve("/api/models/org/repo", "HIT", http.StatusOK, 0)                   // not a download
	out := httptest.NewRecorder()
	m.handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := out.Body.String()
	for cache, want := range map[string]string{"hit": "2", "miss": "1", "stale": "1", "other": "1"} {
		if !strings.Contains(body, `hfproxy_download_ttfb_seconds_count{cache="`+cache+`"} `+want) {
			t.Errorf("cache=%s should have %s sample(s):\n%s", cache, want, body)
		}
	}
	// The miss waited 30 ms before sending headers, so it is in no bucket below that.
	if strings.Contains(body, `hfproxy_download_ttfb_seconds_bucket{cache="miss",le="0.025"} 1`) {
		t.Error("the delay before the first byte was not measured")
	}
	if !strings.Contains(body, `hfproxy_download_ttfb_seconds_bucket{cache="miss",le="0.05"} 1`) {
		t.Errorf("a 30 ms first byte should fit under 50 ms:\n%s", body)
	}
	if strings.Contains(body, "a.bin") || strings.Contains(body, "org/repo") {
		t.Error("repository data leaked into the TTFB metric")
	}
}
