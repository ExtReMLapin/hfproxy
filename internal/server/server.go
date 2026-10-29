package server

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/hfproxy/hfproxy/internal/config"
	"github.com/hfproxy/hfproxy/internal/db"
	"github.com/hfproxy/hfproxy/internal/keyring"
	"github.com/hfproxy/hfproxy/internal/logging"
	"github.com/hfproxy/hfproxy/internal/storage"
	"golang.org/x/oauth2"
)

//go:embed dashboard.html app.css
var dashboardFS embed.FS

//go:embed all:web
var frontendFS embed.FS

type runtimeState struct {
	config       config.Config
	storage      *storage.S3
	httpClient   *http.Client
	oidcProvider *oidc.Provider
	oidcConfig   *oauth2.Config
	oidcVerifier *oidc.IDTokenVerifier
	ring         *keyring.Ring
}

type Server struct {
	runtime              atomic.Pointer[runtimeState]
	db                   *db.Store
	logger               *slog.Logger
	logLevel             *slog.LevelVar
	setupMu              sync.Mutex
	downloadLimitsOnce   sync.Once
	downloadLimiterStore *downloadLimiter
	setupGenerated       string
	setupOut             io.Writer
	adminKnownUntil      atomic.Int64
	preloadsMu           sync.RWMutex
	preloads             map[string]*preloadJob
	preloadSlots         chan struct{}
	stats                statsCache
	loginMu              sync.Mutex
	loginBuckets         map[string]loginBucket
	loginHashSlots       chan struct{}
	negativeMu           sync.Mutex
	negative             map[string]negativeCacheEntry
	missMu               sync.Mutex
	misses               map[string]*cacheMiss
	metrics              *metrics
	inventoryRefreshWake chan struct{}
	fillSlots            chan struct{}
	stagingReserved      atomic.Int64
	breakerMu            sync.Mutex
	breakers             map[string]*circuitBreaker
	apiCacheOnce         sync.Once
	apiCache             *apiCache
	apiFlightMu          sync.Mutex
	apiFlights           map[string]*apiFlight
	maintenanceWake      chan struct{}
	authCacheOnce        sync.Once
	authCacheStore       *authCache
	hitLog               *hitLog
}

type loginBucket struct {
	window time.Time
	count  int
}

const loginRateLimitMaxBuckets = 10_000

type negativeCacheEntry struct {
	expires time.Time
	status  int
	headers http.Header
}

type cacheMiss struct{ done chan struct{} }

type Principal struct {
	ID            string   `json:"id"`
	Username      string   `json:"username"`
	Email         string   `json:"email,omitempty"`
	DisplayName   string   `json:"display_name,omitempty"`
	Role          string   `json:"role"`
	Authenticated bool     `json:"authenticated"`
	TokenID       string   `json:"-"`
	Scopes        []string `json:"-"`
}

// Version is embedded in upstream User-Agent headers and set by release builds.
var Version = "dev"

func New(ctx context.Context, cfg config.Config, store *db.Store, objectStore *storage.S3, logger *slog.Logger) (*Server, error) {
	state, err := makeRuntimeState(ctx, cfg, objectStore, nil)
	if err != nil {
		return nil, err
	}
	if err := configureTracing(ctx, cfg); err != nil {
		return nil, fmt.Errorf("configure trace exporter")
	}
	s := &Server{
		db: store, logger: logger, setupOut: os.Stderr, metrics: newMetrics(), inventoryRefreshWake: make(chan struct{}, 1), preloads: make(map[string]*preloadJob),
		preloadSlots: make(chan struct{}, 2), loginBuckets: make(map[string]loginBucket), loginHashSlots: make(chan struct{}, 4), negative: make(map[string]negativeCacheEntry), misses: make(map[string]*cacheMiss),
	}
	s.runtime.Store(state)
	s.stats.ttl = statsCacheTTL
	if objectStore != nil {
		objectStore.SetObserver(s.metrics)
	}
	slots := cfg.CacheFillConcurrency
	if slots <= 0 {
		slots = defaultFillConcurrency
	}
	s.fillSlots = make(chan struct{}, slots)
	s.maintenanceWake = make(chan struct{}, 1)
	if cfg.StagingDir != "" {
		if err := os.MkdirAll(cfg.StagingDir, 0o700); err != nil {
			return nil, fmt.Errorf("prepare staging directory: %w", err)
		}
	}
	s.startMaintenance(ctx)
	s.startDownloadResumeCleanup(ctx)
	s.startOrphanedBlobGC(ctx)
	s.startCacheInventoryMetrics(ctx)
	return s, nil
}

func (s *Server) startCacheInventoryMetrics(ctx context.Context) {
	refresh := s.currentConfig().CacheInventoryRefresh
	if refresh <= 0 {
		refresh = 5 * time.Minute
	}
	refreshInventory := func() {
		count, size, err := s.db.BlobTotals(ctx)
		if err != nil {
			s.logger.Warn("cache inventory metrics refresh failed")
			return
		}
		s.metrics.setGauge("hfproxy_cache_objects", float64(count))
		s.metrics.setGauge("hfproxy_cache_size_bytes", float64(size))
		if pinned, err := s.pinnedBytes(ctx); err == nil {
			s.metrics.setGauge("hfproxy_cache_pinned_bytes", float64(pinned))
		}
	}
	go func() {
		refreshInventory()
		timer := time.NewTimer(refresh)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				refreshInventory()
				wait := s.currentConfig().CacheInventoryRefresh
				if wait <= 0 {
					wait = 5 * time.Minute
				}
				timer.Reset(wait)
			case <-s.inventoryRefreshWake:
				wait := s.currentConfig().CacheInventoryRefresh
				if wait <= 0 {
					wait = 5 * time.Minute
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(wait)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func makeRuntimeState(ctx context.Context, cfg config.Config, objectStore *storage.S3, previous *runtimeState) (*runtimeState, error) {
	state := &runtimeState{config: cfg, storage: objectStore, httpClient: newHFHTTPClient(cfg), ring: keyring.NewRing(cfg.SessionSecret, cfg.PreviousSessionSecrets...)}
	if cfg.OIDCIssuer != "" {
		var provider *oidc.Provider
		if previous != nil && previous.config.OIDCIssuer == cfg.OIDCIssuer {
			provider = previous.oidcProvider
		} else {
			var err error
			provider, err = oidc.NewProvider(ctx, cfg.OIDCIssuer)
			if err != nil {
				return nil, fmt.Errorf("discover OIDC provider: %w", err)
			}
		}
		state.oidcProvider = provider
		state.oidcConfig = &oauth2.Config{
			ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret,
			RedirectURL: cfg.OIDCRedirectURL,
			Endpoint:    provider.Endpoint(),
			Scopes:      []string{oidc.ScopeOpenID, "profile", "email"},
		}
		state.oidcVerifier = provider.Verifier(&oidc.Config{ClientID: cfg.OIDCClientID})
	}
	return state, nil
}

// keyRing returns the keys derived from the session secret and any previous
// secrets still accepted.
func (s *Server) keyRing() *keyring.Ring {
	state := s.runtime.Load()
	if state.ring != nil {
		return state.ring
	}
	return keyring.NewRing(state.config.SessionSecret, state.config.PreviousSessionSecrets...)
}

func (s *Server) currentConfig() config.Config               { return s.runtime.Load().config }
func (s *Server) currentStorage() *storage.S3                { return s.runtime.Load().storage }
func (s *Server) currentHTTPClient() *http.Client            { return s.runtime.Load().httpClient }
func (s *Server) currentOIDCConfig() *oauth2.Config          { return s.runtime.Load().oidcConfig }
func (s *Server) currentOIDCVerifier() *oidc.IDTokenVerifier { return s.runtime.Load().oidcVerifier }

func (s *Server) applyConfig(ctx context.Context, cfg config.Config, actorID string) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	objects, err := storage.New(ctx, cfg.S3Endpoint, cfg.S3Region, cfg.S3Bucket, cfg.S3Prefix, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3ForcePathStyle, cfg.S3ServerSideEncryption)
	if err != nil {
		return fmt.Errorf("prepare S3 settings")
	}
	previous := s.runtime.Load()
	state, err := makeRuntimeState(ctx, cfg, objects, previous)
	if err != nil {
		return err
	}
	traceProcessor, err := prepareTraceProcessor(ctx, cfg)
	if err != nil {
		return fmt.Errorf("configure trace exporter")
	}
	if objects != nil {
		objects.SetObserver(s.metrics)
	}
	if err := config.SavePersisted(ctx, s.db.SQL, cfg, actorID); err != nil {
		if traceProcessor != nil {
			_ = traceProcessor.Shutdown(ctx)
		}
		return err
	}
	s.runtime.Store(state)
	s.applyLogLevel(cfg.LogLevel)
	activatePreparedTracing(ctx, traceProcessor, cfg)
	select {
	case s.inventoryRefreshWake <- struct{}{}:
	default:
	}
	// A changed cache size limit should take effect now, not at the next tick.
	select {
	case s.maintenanceWake <- struct{}{}:
	default:
	}
	return nil
}

func (s *Server) Handler() http.Handler {
	if s.metrics == nil {
		s.metrics = newMetrics()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/readyz", s.ready)
	mux.HandleFunc("/assets/app.css", s.css)
	mux.HandleFunc("/login", s.login)
	mux.HandleFunc("/logout", s.logout)
	mux.HandleFunc("/auth/login", s.oidcLogin)
	mux.HandleFunc("/auth/callback", s.oidcCallback)
	mux.HandleFunc("/api/v1/auth/login", s.login)
	mux.HandleFunc("/api/v1/auth/callback", s.oidcCallback)
	mux.HandleFunc("/api/v1/auth/logout", s.logout)
	mux.HandleFunc("/api/v1/", s.apiV1)
	mux.HandleFunc("/api/", s.hubAPI)
	mux.HandleFunc("/", s.dashboardOrResolve)
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.currentConfig()
		metricsPath := cfg.MetricsPath
		if metricsPath == "" {
			metricsPath = "/metrics"
		}
		if cfg.MetricsEnabled && metricsOnMainListener(cfg.MetricsAddr) && r.URL.Path == metricsPath {
			s.serveMainListenerMetrics(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
	return s.requestDeadlines(s.securityHeaders(s.traceMiddleware(s.requestLog(s.metrics.middlewareDynamic(func() string {
		cfg := s.currentConfig()
		if !cfg.MetricsEnabled {
			return ""
		}
		if cfg.MetricsPath == "" {
			return "/metrics"
		}
		return cfg.MetricsPath
	}, dispatch)))))
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	defer shutdownTracing()
	server := &http.Server{
		Addr: s.currentConfig().ListenAddr, Handler: s.Handler(),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
		// hfproxy accepts no large uploads, only small JSON and form bodies, so a
		// client that trickles a body in is cut off. There is deliberately no
		// WriteTimeout: downloads of many gigabytes are streamed responses.
		ReadTimeout:    5 * time.Minute,
		MaxHeaderBytes: 1 << 20,
	}
	// Hit events are flushed after the HTTP server has drained, so requests that
	// finish during shutdown still get logged.
	logCtx, stopHitLog := context.WithCancel(context.Background())
	s.startAuthInvalidation(ctx)
	s.startHitLog(logCtx)
	defer func() {
		stopHitLog()
		s.waitHitLog()
	}()
	errCh := make(chan error, 2)
	go func() { errCh <- server.ListenAndServe() }()
	s.logger.Info("http server started", "address", s.currentConfig().ListenAddr)
	s.announceSetup(ctx)
	var metricsServer *http.Server
	if addr := s.currentConfig().MetricsAddr; addr != "" {
		metricsServer = &http.Server{Addr: addr, Handler: s.metricsHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
		go func() { errCh <- metricsServer.ListenAndServe() }()
		s.logger.Info("metrics server started", "address", addr)
	}
	select {
	case err := <-errCh:
		if metricsServer != nil {
			_ = metricsServer.Close()
		}
		_ = server.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if metricsServer != nil {
			_ = metricsServer.Shutdown(shutdownCtx)
		}
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		return <-errCh
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		io.WriteString(w, `{"status":"ok"}`)
	}
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	version, migrationErr := db.MigrationStatus(ctx, s.db.SQL)
	if s.db.SQL.PingContext(ctx) != nil || migrationErr != nil || version < db.LatestMigration || s.currentStorage().Ready(ctx) != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	if s.stagingLow() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "reason": "staging_space"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// serveMainListenerMetrics serves /metrics on the public port. Metrics describe
// traffic and capacity, so here they need an operator or administrator
// (a token with write scope, which Prometheus sends as a bearer token). A
// dedicated HFPROXY_METRICS_ADDR listener, meant for an internal network,
// serves them without credentials.
func (s *Server) serveMainListenerMetrics(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireOperator(w, r); !ok {
		return
	}
	s.metrics.handler().ServeHTTP(w, r)
}

func (s *Server) dashboardOrResolve(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	metricsPath := cfg.MetricsPath
	if metricsPath == "" {
		metricsPath = "/metrics"
	}
	if cfg.MetricsEnabled && metricsOnMainListener(cfg.MetricsAddr) && r.URL.Path == metricsPath {
		s.serveMainListenerMetrics(w, r)
		return
	}
	if r.URL.Path == "/" {
		s.frontend(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/models/") || strings.HasPrefix(r.URL.Path, "/datasets/") || strings.HasPrefix(r.URL.Path, "/spaces/") {
		s.resolveFile(w, r)
		return
	}
	// huggingface_hub omits the repository type for model downloads, using
	// /{repo_id}/resolve/{revision}/{filename}. Treat that standard form as
	// the models equivalent while keeping the SPA fallback for other paths.
	if _, err := parseResolvePath("/models" + r.URL.EscapedPath()); err == nil {
		s.resolveFile(w, r)
		return
	}
	s.frontend(w, r)
}

func (s *Server) frontend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		name = "index.html"
	}
	asset := "web/dist/" + name
	content, err := frontendFS.ReadFile(asset)
	if err != nil {
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		content, err = frontendFS.ReadFile("web/dist/index.html")
		name = "index.html"
	}
	if err != nil {
		content, err = frontendFS.ReadFile("web/index.html")
		name = "index.html"
	}
	if err != nil {
		http.Error(w, "admin interface is not built", http.StatusServiceUnavailable)
		return
	}
	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if name == "index.html" {
		contentType = "text/html; charset=utf-8"
		w.Header().Set("Cache-Control", "no-cache")
		if bytes.Contains(content, []byte(cspNoncePlaceholder)) {
			nonce := randomSecret(18)
			content = bytes.ReplaceAll(content, []byte(cspNoncePlaceholder), []byte(nonce))
			w.Header().Set("Content-Security-Policy", contentSecurityPolicy(nonce))
		}
	} else if immutableAsset(name) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	if r.Method == http.MethodGet {
		_, _ = w.Write(content)
	}
}

func immutableAsset(name string) bool {
	base := path.Base(name)
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 {
		return false
	}
	dash := strings.LastIndexByte(base[:dot], '-')
	if dash < 0 || dot-dash-1 < 8 {
		return false
	}
	for _, r := range base[dash+1 : dot] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *responseRecorder) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
func (w *responseRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}
func (w *responseRecorder) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}
func (w *responseRecorder) Push(target string, opts *http.PushOptions) error {
	p, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return p.Push(target, opts)
}
func (w *responseRecorder) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		if w.status == 0 {
			w.WriteHeader(http.StatusOK)
		}
		n, err := rf.ReadFrom(r)
		w.bytes += n
		return n, err
	}
	return io.Copy(recorderWriterOnly{w}, r)
}

type recorderWriterOnly struct{ io.Writer }

func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := r.Header.Get("X-Request-ID")
		if !validRequestID(rid) {
			rid = randomID()
		}
		w.Header().Set("X-Request-ID", rid)
		recorder := &responseRecorder{ResponseWriter: w}
		info := &requestInfo{ClientIP: s.loginClientIP(r)}
		ctx := context.WithValue(context.WithValue(r.Context(), requestIDKey{}, rid), requestInfoKey{}, info)
		next.ServeHTTP(recorder, r.WithContext(ctx))
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		route := safeRoute(r.URL.Path)
		attrs := []any{"request_id", rid, "method", r.Method, "route", route, "status", status, "duration_ms", time.Since(start).Milliseconds(), "bytes", recorder.bytes,
			"client_ip", info.ClientIP, "user_id", info.UserID, "user_agent", safeLogValue(r.UserAgent(), 200)}
		if info.TokenID != "" {
			attrs = append(attrs, "token_id", info.TokenID)
		}
		if info.Problem != "" {
			attrs = append(attrs, "problem", info.Problem)
		}
		s.logger.Log(r.Context(), requestLogLevel(route, status), "http request", attrs...)
	})
}

type requestIDKey struct{}

// requestInfo carries who made a request, filled in as the request is handled,
// so audit rows and the access log can name the caller.
type requestInfo struct {
	ClientIP string
	UserID   string
	TokenID  string
	// Problem is the title and detail of the error document sent to the
	// client, so the access log can say why a request was refused.
	Problem string
}

// requestLogLevel decides how loudly a finished request is logged: server
// errors are errors, refused or invalid requests are warnings, ordinary
// traffic is info, and the probes and static files that orchestrators and
// browsers hit constantly are only visible at debug level.
func requestLogLevel(route string, status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status == http.StatusUnauthorized || status == http.StatusNotFound:
		// Routine: an unauthenticated visit asks who it is, and clients probe
		// for files that do not exist.
		return slog.LevelInfo
	case status >= 400:
		return slog.LevelWarn
	case route == "/healthz" || route == "/readyz" || strings.HasPrefix(route, "/assets/") || route == "/favicon.svg":
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

type requestInfoKey struct{}

func requestInfoFrom(ctx context.Context) *requestInfo {
	info, _ := ctx.Value(requestInfoKey{}).(*requestInfo)
	return info
}

func requestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}

// validRequestID accepts the identifier a caller or proxy supplied only if it is
// short and made of characters that are safe in logs and headers.
func validRequestID(id string) bool {
	if id == "" || len(id) > 100 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == ':') {
			return false
		}
	}
	return true
}

// safeLogValue bounds a caller-supplied value and drops control characters.
func safeLogValue(value string, max int) string {
	if len(value) > max {
		value = value[:max]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
}

func safeRoute(path string) string {
	if len(path) > 240 {
		return path[:240]
	}
	return path
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		if state := s.runtime.Load(); state != nil && strings.HasPrefix(state.config.PublicURL, "https://") {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy(""))
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=(), interest-cohort=()")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// contentSecurityPolicy is the policy for every response. Styles come from our
// own files only; the console passes a per-response nonce so that the styles
// Angular injects at runtime are allowed without 'unsafe-inline'.
func contentSecurityPolicy(styleNonce string) string {
	style := "style-src 'self'"
	if styleNonce != "" {
		style += " 'nonce-" + styleNonce + "'"
	}
	return "default-src 'self'; " + style + "; form-action 'self'; frame-ancestors 'none'; base-uri 'self'"
}

// cspNoncePlaceholder stands for the nonce in the built index.html
// (ngCspNonce on the root element).
const cspNoncePlaceholder = "__CSP_NONCE__"

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	if info := requestInfoFrom(r.Context()); info != nil {
		info.Problem = safeLogValue(title+": "+detail, 300)
	}
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "about:blank", "title": title, "status": status,
		"detail": detail, "request_id": requestID(r.Context()),
	})
}

func methodNotAllowed(w http.ResponseWriter) {
	methodNotAllowedWith(w, "GET, HEAD, POST, PATCH, DELETE")
}

func methodNotAllowedWith(w http.ResponseWriter, methods string) {
	w.Header().Set("Allow", methods)
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(http.StatusMethodNotAllowed)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "about:blank", "title": "Method not allowed", "status": http.StatusMethodNotAllowed,
	})
}

func (s *Server) audit(ctx context.Context, actorID, action, targetType, targetID string, metadata map[string]any) {
	if metadata == nil {
		metadata = map[string]any{}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil || len(encoded) > 4096 {
		encoded = []byte(`{}`)
	}
	err = s.db.InsertAudit(ctx, actorID, action, targetType, targetID, string(encoded), requestID(ctx))
	if err != nil {
		s.logger.Error("audit write failed", "request_id", requestID(ctx), "action", action, "error", safeLogValue(err.Error(), 300))
		return
	}
	if action == "auth.login_failed" {
		targetID = "" // the name that was typed is kept in the audit table only
	}
	s.logger.Info("audit event", "request_id", requestID(ctx), "actor_id", actorID, "action", action, "target_type", targetType, "target_id", safeLogValue(targetID, 128))
}

func (s *Server) authenticate(r *http.Request) (*Principal, error) {
	p, err := s.authenticateCredentials(r)
	if err == nil && p != nil {
		if info := requestInfoFrom(r.Context()); info != nil {
			info.UserID, info.TokenID = p.ID, p.TokenID
		}
		via := "session"
		if p.TokenID != "" {
			via = "token"
		}
		s.logger.Debug("request authenticated", "request_id", requestID(r.Context()), "user_id", p.ID, "role", p.Role, "via", via)
	}
	return p, err
}

func (s *Server) authenticateCredentials(r *http.Request) (*Principal, error) {
	if p, ok := r.Context().Value(localPrincipalContextKey{}).(*Principal); ok && p != nil {
		principal := *p
		return &principal, nil
	}
	if header := r.Header.Get("Authorization"); header != "" {
		parts := strings.Fields(header)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return nil, errUnauthenticated
		}
		token := parts[1]
		if len(token) < 24 || strings.ContainsAny(token, " \t\r\n") {
			return nil, errUnauthenticated
		}
		cacheKey := authCacheKey("token", token)
		if cached, ok := s.authLookups().get(cacheKey); ok {
			return cached, nil
		}
		ring := s.keyRing()
		found, err := s.db.LookupToken(r.Context(), ring.HashCandidates(token))
		if err != nil || found.Status != "active" {
			return nil, errUnauthenticated
		}
		p := Principal{ID: found.ID, Username: found.Username, Email: found.Email, DisplayName: found.DisplayName, Role: found.Role, TokenID: found.TokenID}
		scopes, storedHash := found.Scopes, found.StoredHash
		p.Authenticated = true
		p.Scopes = scopes
		if !ring.IsCurrent(token, storedHash) {
			// Hashed under an older scheme or secret: move it to the current key
			// now that the plaintext is in hand.
			if err := s.db.RehashToken(r.Context(), p.TokenID, storedHash, ring.Hash(token), ring.KeyID()); err != nil {
				s.logger.Warn("token hash could not be moved to the current key", "request_id", requestID(r.Context()))
			}
		}
		if err := s.db.TouchToken(r.Context(), p.TokenID); err != nil {
			s.logger.Warn("token last use could not be recorded", "request_id", requestID(r.Context()))
		}
		s.authLookups().put(cacheKey, &p)
		return &p, nil
	}
	cookie, err := r.Cookie("hfproxy_session")
	if err != nil {
		return nil, errNoCredentials
	}
	if cookie.Value == "" {
		return nil, errUnauthenticated
	}
	sessionKey := authCacheKey("session", cookie.Value)
	if cached, ok := s.authLookups().get(sessionKey); ok {
		return cached, nil
	}
	found, err := s.db.LookupSession(r.Context(), s.keyRing().HashCandidates(cookie.Value))
	if err != nil || found.Status != "active" {
		return nil, errUnauthenticated
	}
	p := Principal{ID: found.ID, Username: found.Username, Email: found.Email, DisplayName: found.DisplayName, Role: found.Role, Authenticated: true}
	if err := s.db.TouchSession(r.Context(), found.Hash, s.currentConfig().SessionIdleTTL, s.currentConfig().SessionAbsoluteTTL); err != nil {
		s.logger.Warn("session activity could not be recorded", "request_id", requestID(r.Context()))
	}
	s.authLookups().put(sessionKey, &p)
	return &p, nil
}

// authLookups returns the short-lived cache of credential lookups, created on
// first use with the configured TTL.
func (s *Server) authLookups() *authCache {
	s.authCacheOnce.Do(func() { s.authCacheStore = newAuthCache(s.currentConfig().AuthCacheTTL) })
	return s.authCacheStore
}

func hasBearer(r *http.Request) bool {
	parts := strings.Fields(r.Header.Get("Authorization"))
	return len(parts) == 2 && strings.EqualFold(parts[0], "Bearer")
}

func (s *Server) createSession(ctx context.Context, userID string, w http.ResponseWriter, r *http.Request) (string, error) {
	// A login always starts a fresh session: drop the one the browser still
	// holds so a pre-login session identifier can never survive authentication.
	if r != nil {
		if old, err := r.Cookie("hfproxy_session"); err == nil && old.Value != "" {
			if err := s.db.DeleteSessions(ctx, s.keyRing().HashCandidates(old.Value)); err != nil {
				s.logger.Warn("the previous session could not be ended at sign-in", "request_id", requestID(ctx))
			}
		}
	}
	token := randomSecret(32)
	csrf := randomSecret(32)
	ring := s.keyRing()
	err := s.db.CreateSession(ctx, ring.Hash(token), userID, ring.Hash(csrf), time.Now().Add(s.currentConfig().SessionIdleTTL), ring.KeyID())
	if err != nil {
		return "", err
	}
	for _, cookie := range newSessionCookies(token, csrf, s.currentConfig()) {
		http.SetCookie(w, cookie)
	}
	return csrf, nil
}

func newSessionCookies(token, csrf string, cfg config.Config) []*http.Cookie {
	secure := strings.HasPrefix(cfg.PublicURL, "https://") || strings.HasPrefix(cfg.OIDCRedirectURL, "https://")
	maxAge := int(cfg.SessionAbsoluteTTL.Seconds())
	return []*http.Cookie{
		{Name: "hfproxy_session", Value: token, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: maxAge},
		{Name: "hfproxy_csrf", Value: csrf, Path: "/", Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: maxAge},
	}
}

func (s *Server) checkCSRF(r *http.Request) bool {
	csrfCookie, err := r.Cookie("hfproxy_csrf")
	if err != nil || csrfCookie.Value == "" {
		return false
	}
	provided := r.FormValue("csrf_token")
	if provided == "" {
		provided = r.Header.Get("X-CSRF-Token")
	}
	if provided == "" || !constantEqual(provided, csrfCookie.Value) {
		return false
	}
	cookie, err := r.Cookie("hfproxy_session")
	if err != nil {
		return false
	}
	ring := s.keyRing()
	stored, err := s.db.SessionCSRFHash(r.Context(), ring.HashCandidates(cookie.Value))
	if err != nil {
		return false
	}
	return ring.MatchesAny(provided, stored)
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (*Principal, bool) {
	return s.requireRole(w, r, "Administrator role required.", "admin")
}

func (s *Server) requireOperator(w http.ResponseWriter, r *http.Request) (*Principal, bool) {
	return s.requireRole(w, r, "Operator or administrator role required.", "admin", "operator")
}

// requireRole authenticates the request and checks that the principal holds one
// of the roles and, for tokens, a scope that allows administration.
func (s *Server) requireRole(w http.ResponseWriter, r *http.Request, denied string, roles ...string) (*Principal, bool) {
	p, err := s.authenticate(r)
	if err != nil {
		s.logger.Debug("administration request without valid credentials", "request_id", requestID(r.Context()), "route", safeRoute(r.URL.Path), "reason", err.Error())
		w.Header().Set("WWW-Authenticate", `Bearer realm="hfproxy"`)
		writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "Provide a valid hfproxy token or sign in.")
		return nil, false
	}
	if !slices.Contains(roles, p.Role) {
		s.logger.Warn("access denied: role not allowed", "request_id", requestID(r.Context()), "user_id", p.ID, "role", p.Role, "required", roles, "method", r.Method, "route", safeRoute(r.URL.Path))
		writeProblem(w, r, http.StatusForbidden, "Forbidden", denied)
		return nil, false
	}
	if !tokenAllowsAdministration(p) {
		s.logger.Warn("access denied: read-only token", "request_id", requestID(r.Context()), "user_id", p.ID, "token_id", p.TokenID, "method", r.Method, "route", safeRoute(r.URL.Path))
		writeProblem(w, r, http.StatusForbidden, "Forbidden", "This token is read-only and cannot use the administration API.")
		return nil, false
	}
	return p, true
}

// tokenAllowsAdministration keeps read-scoped tokens out of the administration
// API even when their owner is an administrator. Browser sessions carry no
// token and are governed by role alone.
func tokenAllowsAdministration(p *Principal) bool {
	return p.TokenID == "" || hasScope(p, "write") || hasScope(p, "admin")
}

var errUnauthenticated = errors.New("unauthenticated")
var errNoCredentials = errors.New("no credentials")

// fail answers with a problem document and records the cause, which the client
// never sees. Server errors are logged at error level, upstream failures such
// as an unreachable Hub or object store at warn level.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, cause error, status int, title, detail string) {
	attrs := []any{"request_id", requestID(r.Context()), "method", r.Method, "route", safeRoute(r.URL.Path), "status", status, "title", title}
	if cause != nil {
		attrs = append(attrs, "error", safeLogValue(cause.Error(), 500))
	}
	if status >= http.StatusInternalServerError && status != http.StatusBadGateway && status != http.StatusServiceUnavailable {
		s.logger.Error("request failed", attrs...)
	} else {
		s.logger.Warn("request could not be completed", attrs...)
	}
	writeProblem(w, r, status, title, detail)
}

// SetLogLevelVariable gives the server the variable that controls its logger,
// so a level saved in the console takes effect without a restart.
func (s *Server) SetLogLevelVariable(level *slog.LevelVar) { s.logLevel = level }

func (s *Server) applyLogLevel(name string) {
	if s.logLevel == nil {
		return
	}
	next, err := logging.Normalize(name)
	previous := logging.Name(s.logLevel.Level())
	if err != nil || next == previous {
		return
	}
	// Logged before the switch, at warn, so the change shows in the old level's
	// output and leaves a trace when the level is raised.
	s.logger.Warn("log level changed", "from", previous, "to", next)
	logging.Set(s.logLevel, next)
}
