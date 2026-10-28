package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hfproxy/hfproxy/internal/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestTracingDisabledAndEnabled(t *testing.T) {
	processor, err := prepareTraceProcessor(context.Background(), config.Config{})
	if err != nil || processor != nil {
		t.Fatalf("disabled processor = %v, %v", processor, err)
	}
	var exported atomic.Bool
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("OTLP method = %s", r.Method)
		}
		if r.URL.Path != "/v1/traces" {
			t.Errorf("OTLP path = %s", r.URL.Path)
		}
		exported.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	cfg := config.Config{TracingEnabled: true, TracingServiceName: "hfproxy-test", TracingSamplingRatio: 1, OTLPEndpoint: collector.URL}
	processor, err = prepareTraceProcessor(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	activatePreparedTracing(context.Background(), processor, cfg)
	_, span := otel.Tracer("test").Start(context.Background(), "sampled")
	span.End()
	if err := configureTracing(context.Background(), config.Config{}); err != nil {
		t.Fatal(err)
	}
	if !exported.Load() {
		t.Fatal("trace exporter did not send a span")
	}
}

func TestTraceResourceUsesConfiguredServiceNameOnly(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	wrapped := &serviceResourceExporter{SpanExporter: exporter, resource: resource.NewSchemaless(attribute.String("service.name", "hfproxy-admin-configured"))}
	provider := trace.NewTracerProvider(trace.WithSyncer(wrapped), trace.WithSampler(trace.AlwaysSample()))
	_, span := provider.Tracer("test").Start(context.Background(), "operation")
	span.End()
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d", len(spans))
	}
	attrs := spans[0].Resource.Attributes()
	if len(attrs) != 1 || attrs[0].Key != attribute.Key("service.name") || attrs[0].Value.AsString() != "hfproxy-admin-configured" {
		t.Fatalf("span resource = %#v", attrs)
	}
	_ = provider.Shutdown(context.Background())
}

func TestIncomingTraceUsesNormalizedRouteAndW3CParent(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	processor := trace.NewSimpleSpanProcessor(exporter)
	cfg := config.Config{TracingEnabled: true, TracingSamplingRatio: 1, TracingServiceName: "test"}
	activatePreparedTracing(context.Background(), processor, cfg)
	provider := otel.GetTracerProvider()
	oldPropagator := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		_ = configureTracing(context.Background(), config.Config{})
		otel.SetTextMapPropagator(oldPropagator)
	})
	s := &Server{}
	s.runtime.Store(&runtimeState{config: config.Config{TracingEnabled: true}})
	h := s.traceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusPartialContent) }))
	parentCtx, parent := provider.Tracer("test").Start(context.Background(), "parent")
	parentSC := parent.SpanContext()
	parent.End()
	req := httptest.NewRequest(http.MethodGet, "/models/private-org/private-repo/resolve/hidden-revision/secret-file?token=secret", nil)
	propagation.TraceContext{}.Inject(parentCtx, propagation.HeaderCarrier(req.Header))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	spans := exporter.GetSpans()
	var serverSpan *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "http.server" {
			serverSpan = &spans[i]
		}
	}
	if serverSpan == nil {
		t.Fatal("incoming server span was not exported")
	}
	if !serverSpan.Parent.IsValid() || serverSpan.Parent.SpanID() != parentSC.SpanID() {
		t.Fatalf("server span parent = %v, want %v", serverSpan.Parent, parentSC)
	}
	var route string
	for _, a := range serverSpan.Attributes {
		if a.Key == attribute.Key("http.route") {
			route = a.Value.AsString()
		}
	}
	if route != "/models/{repo}/resolve/{revision}/{file}" {
		t.Fatalf("http.route = %q", route)
	}
	attrs := strings.ToLower(serverSpan.Name)
	for _, a := range serverSpan.Attributes {
		attrs += strings.ToLower(a.Value.String())
	}
	if strings.Contains(attrs, "private-repo") || strings.Contains(attrs, "secret-file") || strings.Contains(attrs, "hidden-revision") || strings.Contains(attrs, "token=secret") {
		t.Fatal("sensitive request details appeared on the span")
	}
}
