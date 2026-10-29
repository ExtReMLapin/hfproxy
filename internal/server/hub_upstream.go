package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hfproxy/hfproxy/internal/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type idleBody struct {
	body    io.ReadCloser
	timeout time.Duration
	mu      sync.Mutex
	timer   *time.Timer
	closed  bool
}

type flushWriter struct {
	io.Writer
	flusher http.Flusher
}

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err == nil {
		w.flusher.Flush()
	}
	return n, err
}

func asStreamingWriter(w io.Writer) io.Writer {
	if flusher, ok := w.(http.Flusher); ok {
		return flushWriter{Writer: w, flusher: flusher}
	}
	return w
}

func newIdleBody(body io.ReadCloser, timeout time.Duration) *idleBody {
	return &idleBody{body: body, timeout: timeout}
}

func (b *idleBody) resetTimer() {
	if b.timeout <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	if b.timer == nil {
		b.timer = time.AfterFunc(b.timeout, func() {
			b.mu.Lock()
			if b.closed {
				b.mu.Unlock()
				return
			}
			b.closed = true
			b.mu.Unlock()
			_ = b.body.Close()
		})
	} else {
		b.timer.Reset(b.timeout)
	}
}

func (b *idleBody) stopTimer() {
	b.mu.Lock()
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.resetTimer()
	defer b.stopTimer()
	return b.body.Read(p)
}

func (b *idleBody) Close() error {
	b.mu.Lock()
	b.closed = true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
	return b.body.Close()
}

// upstreamPortAllowed limits the ports the proxy will connect to. HTTPS goes to
// 443, loopback development may use 80, and the host:port of the configured
// Hub URL is always allowed, so a mirror or an artifact repository listening
// on another port (for example https://mirror.example.com:8443) works. A
// redirect cannot reach any other non-standard port.
func upstreamPortAllowed(u, base *url.URL) bool {
	port := u.Port()
	if port == "" || port == "443" {
		return true
	}
	if port == "80" && config.IsLoopbackHost(u.Hostname()) {
		return true
	}
	return base != nil && base.Port() == port && strings.EqualFold(base.Hostname(), u.Hostname())
}

func (s *Server) fetchHub(ctx context.Context, method, requestPath string, query url.Values, headers http.Header) (*http.Response, http.Header, error) {
	target := strings.TrimRight(s.currentConfig().HFBaseURL, "/") + requestPath
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	baseURL, _ := url.Parse(s.currentConfig().HFBaseURL)
	baseHost := strings.ToLower(baseURL.Hostname())
	var first http.Header
	for hops := 0; hops <= 5; hops++ {
		u, err := url.Parse(target)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid upstream URL")
		}
		if u.Scheme != "https" && !(u.Scheme == "http" && config.IsLoopbackHost(u.Hostname())) {
			return nil, nil, fmt.Errorf("unsafe upstream redirect")
		}
		if u.User != nil || u.Host == "" {
			return nil, nil, fmt.Errorf("unsafe upstream URL")
		}
		if !upstreamPortAllowed(u, baseURL) {
			return nil, nil, fmt.Errorf("unsafe upstream port")
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
		if err != nil {
			return nil, nil, fmt.Errorf("build upstream request")
		}
		req.Header.Set("User-Agent", "hfproxy/"+Version)
		req.Header.Set("Accept-Encoding", "identity")
		for key, values := range headers {
			if isHopByHop(key) || strings.HasPrefix(strings.ToLower(key), "proxy-") {
				continue
			}
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
		if s.currentConfig().HFToken != "" && strings.EqualFold(u.Hostname(), baseHost) {
			req.Header.Set("Authorization", "Bearer "+s.currentConfig().HFToken)
		}
		operation := upstreamOperation(method, requestPath)
		cfg := s.currentConfig()
		upstreamHost := strings.ToLower(u.Hostname())
		breaker := s.upstreamBreaker(upstreamHost)
		if !breaker.Allow(time.Now(), cfg.UpstreamBreakerThreshold) {
			if s.metrics != nil {
				s.metrics.add("hfproxy_upstream_breaker_rejected_total", []string{"host=" + upstreamHost}, 1)
			}
			return nil, nil, errUpstreamCircuitOpen
		}
		spanCtx, span := otel.Tracer("github.com/hfproxy/hfproxy/internal/server").Start(ctx, "upstream.request", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attribute.String("hfproxy.operation", operation)))
		otel.GetTextMapPropagator().Inject(spanCtx, propagation.HeaderCarrier(req.Header))
		req = req.WithContext(spanCtx)
		attemptStarted := time.Now()
		resp, err := s.currentHTTPClient().Do(req)
		for attempt := 0; attempt < maxUpstreamRetries && retryableUpstream(ctx, resp, err); attempt++ {
			wait := upstreamRetryDelay(attempt, resp)
			if resp != nil {
				resp.Body.Close()
			}
			if s.metrics != nil {
				s.metrics.add("hfproxy_upstream_retries_total", []string{"operation=" + operation}, 1)
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
			}
			if ctx.Err() != nil {
				break
			}
			resp, err = s.currentHTTPClient().Do(req)
		}
		switch {
		case ctx.Err() != nil:
			breaker.Abort()
		case retryableUpstream(ctx, resp, err):
			if breaker.Failure(time.Now(), cfg.UpstreamBreakerThreshold, cfg.UpstreamBreakerCooldown) {
				s.logger.Warn("upstream circuit opened", "host", upstreamHost, "cooldown", cfg.UpstreamBreakerCooldown.String())
				if s.metrics != nil {
					s.metrics.add("hfproxy_upstream_breaker_opened_total", []string{"host=" + upstreamHost}, 1)
				}
			}
		default:
			breaker.Success()
		}
		if err != nil {
			span.SetAttributes(attribute.String("error.type", upstreamErrorReason(ctx, err)))
			span.SetStatus(codes.Error, "upstream request failed")
			span.End()
			if s.metrics != nil {
				reason := upstreamErrorReason(ctx, err)
				s.metrics.add("hfproxy_upstream_requests_total", []string{"operation=" + operation, "status_class=error"}, 1)
				s.metrics.add("hfproxy_upstream_errors_total", []string{"reason=" + reason}, 1)
				s.metrics.observe("hfproxy_upstream_request_duration_seconds", []string{"operation=" + operation}, time.Since(attemptStarted).Seconds())
			}
			// The cause stays in the log (without the URL, which can carry a
			// signature); the client only learns that the upstream failed.
			cause := err
			var urlErr *url.Error
			if errors.As(err, &urlErr) {
				cause = urlErr.Err
			}
			s.logger.Warn("upstream request error", "request_id", requestID(ctx), "host", upstreamHost, "operation", operation, "reason", upstreamErrorReason(ctx, err), "error", cause.Error())
			return nil, nil, fmt.Errorf("upstream request failed")
		}
		if s.metrics != nil {
			class := strconv.Itoa(resp.StatusCode/100) + "xx"
			s.metrics.add("hfproxy_upstream_requests_total", []string{"operation=" + operation, "status_class=" + class}, 1)
			s.metrics.observe("hfproxy_upstream_request_duration_seconds", []string{"operation=" + operation}, time.Since(attemptStarted).Seconds())
			if resp.StatusCode >= 400 {
				s.metrics.add("hfproxy_upstream_errors_total", []string{"reason=http_" + class}, 1)
			}
			resp.Body = &countingBody{ReadCloser: resp.Body, onRead: func(n int) {
				if n > 0 {
					s.metrics.add("hfproxy_upstream_bytes_received_total", []string{"operation=" + operation}, float64(n))
				}
			}}
		}
		span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
		if resp.StatusCode >= 400 {
			span.SetStatus(codes.Error, "upstream HTTP failure")
		}
		spanEnded := false
		endSpan := func(readErr error) {
			if !spanEnded {
				if readErr != nil && readErr != io.EOF {
					span.SetStatus(codes.Error, "upstream response read failed")
				}
				spanEnded = true
				span.End()
			}
		}
		if body, ok := resp.Body.(*countingBody); ok {
			body.onClose = endSpan
		} else {
			resp.Body = &countingBody{ReadCloser: resp.Body, onRead: func(int) {}, onClose: endSpan}
		}
		if first == nil {
			first = resp.Header.Clone()
		}
		if !isRedirectStatus(resp.StatusCode) {
			resp.Body = newIdleBody(resp.Body, s.currentConfig().HFTimeout)
			return resp, first, nil
		}
		location := resp.Header.Get("Location")
		resp.Body.Close()
		if location == "" {
			return nil, first, fmt.Errorf("upstream redirect did not include a target")
		}
		if hops == 5 {
			return nil, first, fmt.Errorf("upstream redirect limit exceeded")
		}
		next, err := u.Parse(location)
		if err != nil {
			return nil, first, fmt.Errorf("invalid upstream redirect")
		}
		host := strings.ToLower(next.Hostname())
		if !s.allowedRedirectHost(host) || next.User != nil || next.Scheme != "https" && !(next.Scheme == "http" && config.IsLoopbackHost(next.Hostname())) {
			return nil, first, fmt.Errorf("upstream redirect host is not allowed")
		}
		if !upstreamPortAllowed(next, baseURL) {
			return nil, first, fmt.Errorf("unsafe upstream redirect port")
		}
		target = next.String()
	}
	return nil, first, fmt.Errorf("upstream redirect limit exceeded")
}

// isRedirectStatus reports whether a response asks the client to go elsewhere.
// The other 3xx statuses, notably 304 Not Modified, are final answers.
func isRedirectStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

const maxUpstreamRetries = 2

// upstreamRetryBase is the first retry delay; it doubles for each retry.
var upstreamRetryBase = 250 * time.Millisecond

// retryableUpstream reports whether a failed attempt is worth repeating. Every
// request fetchHub makes is a body-less GET or HEAD, so repeating is safe.
func retryableUpstream(ctx context.Context, resp *http.Response, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		return upstreamErrorReason(ctx, err) != "cancelled"
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// upstreamRetryDelay doubles the base delay per attempt, adds jitter, and
// honors a short Retry-After from the Hub.
func upstreamRetryDelay(attempt int, resp *http.Response) time.Duration {
	delay := upstreamRetryBase << attempt
	if resp != nil {
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
			if hinted := time.Duration(seconds) * time.Second; hinted > delay {
				delay = hinted
			}
		}
	}
	if delay > 5*time.Second {
		delay = 5 * time.Second
	}
	return delay + time.Duration(rand.Int64N(int64(delay)/4+1))
}

func upstreamOperation(method, path string) string {
	if method == http.MethodHead {
		return "metadata"
	}
	if strings.Contains(path, "/resolve/") {
		return "download"
	}
	if strings.Contains(path, "/revision/") {
		return "resolve"
	}
	if strings.HasPrefix(path, "/api/") {
		return "metadata"
	}
	return "other"
}
func upstreamErrorReason(ctx context.Context, err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "connection"
	}
	return "other"
}

type countingBody struct {
	io.ReadCloser
	onRead    func(int)
	onClose   func(error)
	closeOnce sync.Once
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.onRead(n)
	return n, err
}

func (b *countingBody) Close() error {
	err := b.ReadCloser.Close()
	b.closeOnce.Do(func() {
		if b.onClose != nil {
			b.onClose(err)
		}
	})
	return err
}

func newEgressDialer(policy egressPolicy, proxy *url.URL) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if proxy != nil {
			proxyScheme := strings.ToLower(proxy.Scheme)
			if strings.HasPrefix(proxyScheme, "socks4") || strings.HasPrefix(proxyScheme, "socks5") {
				return dialSOCKS(ctx, network, address, dialer, proxy, policy)
			}
			proxyPort := proxy.Port()
			if proxyPort == "" {
				proxyPort = "80"
				if proxyScheme == "https" {
					proxyPort = "443"
				}
			}
			proxyHost := proxy.Hostname()
			sameHost := strings.EqualFold(host, proxyHost)
			if hostIP, hostErr := netip.ParseAddr(host); hostErr == nil {
				if proxyIP, proxyErr := netip.ParseAddr(proxyHost); proxyErr == nil {
					sameHost = hostIP.Unmap() == proxyIP.Unmap()
				}
			}
			if !sameHost || port != proxyPort {
				return nil, fmt.Errorf("egress target does not match the configured Hugging Face proxy")
			}
			return dialConfiguredProxy(ctx, network, dialer, proxy)
		}
		checkAndDial := func(ip netip.Addr) (net.Conn, error) {
			ip = ip.Unmap()
			allowed := policy.allows(ip)
			if !allowed {
				return nil, fmt.Errorf("egress to non-public IP is blocked")
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}
		if ip, err := netip.ParseAddr(host); err == nil {
			return checkAndDial(ip)
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, candidate := range addresses {
			ip, ok := netip.AddrFromSlice(candidate.IP)
			if !ok {
				continue
			}
			conn, dialErr := checkAndDial(ip)
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("upstream host resolved to no allowed IP addresses")
		}
		return nil, lastErr
	}
}
func newHFHTTPClient(cfg config.Config) *http.Client {
	base, _ := url.Parse(cfg.HFBaseURL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	var proxy *url.URL
	if cfg.HFProxyURL != "" {
		proxy, _ = url.Parse(cfg.HFProxyURL)
		proxy.Scheme = strings.ToLower(proxy.Scheme)
		switch proxy.Scheme {
		case "http", "https":
			transport.Proxy = http.ProxyURL(proxy)
		default:
			transport.Proxy = nil
		}
	} else {
		// Do not inherit HTTP(S)_PROXY from the process environment unless it
		// was explicitly configured for Hugging Face.
		transport.Proxy = nil
	}
	transport.DialContext = newEgressDialer(egressPolicy{allowLoopback: config.IsLoopbackHost(base.Hostname()), private: cfg.HFAllowedPrivateNets}, proxy)
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	transport.ResponseHeaderTimeout = cfg.HFTimeout
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func (s *Server) allowedRedirectHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	base, _ := url.Parse(s.currentConfig().HFBaseURL)
	if strings.TrimSuffix(strings.ToLower(base.Hostname()), ".") == host {
		return true
	}
	if ip, err := netip.ParseAddr(host); err == nil && !ip.IsGlobalUnicast() {
		return false
	}
	for _, allowed := range s.currentConfig().HFAllowedHosts {
		allowed = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(allowed)), ".")
		if strings.HasPrefix(allowed, "*.") {
			suffix := strings.TrimPrefix(allowed, "*")
			if strings.HasSuffix(host, suffix) && host != strings.TrimPrefix(suffix, ".") {
				return true
			}
		} else if strings.EqualFold(host, allowed) {
			return true
		}
	}
	return false
}
func isHopByHop(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "host":
		return true
	}
	return false
}
