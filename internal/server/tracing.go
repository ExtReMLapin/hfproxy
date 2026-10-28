package server

import (
	"bufio"
	"context"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nicolas/hfproxy/internal/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type dynamicSpanProcessor struct {
	mu      sync.RWMutex
	current sdktrace.SpanProcessor
}

func (p *dynamicSpanProcessor) OnStart(ctx context.Context, span sdktrace.ReadWriteSpan) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.current != nil {
		p.current.OnStart(ctx, span)
	}
}
func (p *dynamicSpanProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.current != nil {
		p.current.OnEnd(span)
	}
}
func (p *dynamicSpanProcessor) Shutdown(ctx context.Context) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.current != nil {
		return p.current.Shutdown(ctx)
	}
	return nil
}
func (p *dynamicSpanProcessor) ForceFlush(ctx context.Context) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.current != nil {
		return p.current.ForceFlush(ctx)
	}
	return nil
}
func (p *dynamicSpanProcessor) replace(next sdktrace.SpanProcessor) sdktrace.SpanProcessor {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.current
	p.current = next
	return old
}

type dynamicSampler struct{ ratio atomic.Uint64 }

func (s *dynamicSampler) set(ratio float64)   { s.ratio.Store(math.Float64bits(ratio)) }
func (s *dynamicSampler) Description() string { return "hfproxy.ParentBasedRatioSampler" }
func (s *dynamicSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	if parent := trace.SpanContextFromContext(p.ParentContext); parent.IsValid() {
		if parent.IsSampled() {
			return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample}
		}
		return sdktrace.SamplingResult{Decision: sdktrace.Drop}
	}
	ratio := math.Float64frombits(s.ratio.Load())
	return sdktrace.TraceIDRatioBased(ratio).ShouldSample(p)
}

type tracingRuntime struct {
	once      sync.Once
	processor *dynamicSpanProcessor
	sampler   *dynamicSampler
	provider  *sdktrace.TracerProvider
}

var traces tracingRuntime

func ensureTracingRuntime() {
	traces.once.Do(func() {
		traces.processor = &dynamicSpanProcessor{}
		traces.sampler = &dynamicSampler{}
		traces.provider = sdktrace.NewTracerProvider(sdktrace.WithSampler(traces.sampler), sdktrace.WithSpanProcessor(traces.processor))
		otel.SetTracerProvider(traces.provider)
		otel.SetTextMapPropagator(propagation.TraceContext{})
	})
}

func prepareTraceProcessor(ctx context.Context, cfg config.Config) (sdktrace.SpanProcessor, error) {
	if !cfg.TracingEnabled {
		return nil, nil
	}
	endpoint := cfg.OTLPEndpoint
	if u, err := url.Parse(endpoint); err == nil && (u.Path == "" || u.Path == "/") {
		u.Path = "/v1/traces"
		endpoint = u.String()
	}
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, err
	}
	serviceResource := resource.NewSchemaless(attribute.String("service.name", cfg.TracingServiceName))
	return sdktrace.NewBatchSpanProcessor(&serviceResourceExporter{SpanExporter: exporter, resource: serviceResource}), nil
}

type serviceResourceExporter struct {
	sdktrace.SpanExporter
	resource *resource.Resource
}

func (e *serviceResourceExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	wrapped := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, span := range spans {
		wrapped[i] = serviceResourceSpan{ReadOnlySpan: span, resource: e.resource}
	}
	return e.SpanExporter.ExportSpans(ctx, wrapped)
}

type serviceResourceSpan struct {
	sdktrace.ReadOnlySpan
	resource *resource.Resource
}

func (s serviceResourceSpan) Resource() *resource.Resource { return s.resource }

func configureTracing(ctx context.Context, cfg config.Config) error {
	processor, err := prepareTraceProcessor(ctx, cfg)
	if err != nil {
		return err
	}
	activatePreparedTracing(ctx, processor, cfg)
	return nil
}

func activatePreparedTracing(ctx context.Context, processor sdktrace.SpanProcessor, cfg config.Config) {
	ensureTracingRuntime()
	old := traces.processor.replace(processor)
	ratio := cfg.TracingSamplingRatio
	if !cfg.TracingEnabled {
		ratio = 0
	}
	traces.sampler.set(ratio)
	if old != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = old.ForceFlush(shutdownCtx)
		_ = old.Shutdown(shutdownCtx)
		cancel()
	}
}

func shutdownTracing() {
	ensureTracingRuntime()
	old := traces.processor.replace(nil)
	traces.sampler.set(0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if old != nil {
		_ = old.ForceFlush(ctx)
		_ = old.Shutdown(ctx)
	}
	_ = traces.provider.Shutdown(ctx)
}

func (s *Server) traceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.currentConfig()
		metricsPath:=cfg.MetricsPath;if metricsPath==""{metricsPath="/metrics"}
		if !cfg.TracingEnabled || (cfg.MetricsEnabled&&r.URL.Path==metricsPath) {
			next.ServeHTTP(w, r)
			return
		}
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		route := routeTemplate(r.URL.Path)
		ctx, span := otel.Tracer("github.com/nicolas/hfproxy/internal/server").Start(ctx, "http.server", trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attribute.String("http.request.method", r.Method), attribute.String("http.route", route)))
		wrapped := &traceResponseWriter{ResponseWriter: w}
		defer func() {
			status := wrapped.status
			if status == 0 {
				status = http.StatusOK
			}
			span.SetAttributes(attribute.Int("http.response.status_code", status))
			if status >= 500 {
				span.SetStatus(codes.Error, "server error")
			}
			span.End()
		}()
		next.ServeHTTP(wrapped, r.WithContext(ctx))
	})
}

type traceResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *traceResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *traceResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}
func (w *traceResponseWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *traceResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}
func (w *traceResponseWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}
func (w *traceResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return io.Copy(traceWriterOnly{w.ResponseWriter}, r)
}

type traceWriterOnly struct{ io.Writer }
