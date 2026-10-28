package storage

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func startStorageSpan(ctx context.Context, operation string) (context.Context, trace.Span) {
	return otel.Tracer("github.com/hfproxy/hfproxy/internal/storage").Start(ctx, "storage."+operation,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("storage.operation", operation)),
	)
}

func finishStorageSpan(span trace.Span, err error) {
	if err != nil {
		span.SetStatus(codes.Error, "storage operation failed")
	}
	span.End()
}
