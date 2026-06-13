// SPDX-License-Identifier: Apache-2.0
package api

import (
	"net/http"

	"github.com/pandastack/agent/internal/obs"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// otelTracing extracts incoming W3C traceparent (so api->agent traces
// connect), starts a server span per request, and records standard HTTP
// attributes. Spans cover the full middleware chain.
//
// We deliberately use HTTP-server semconv attributes so any OTel-aware
// backend (Tempo, Jaeger, Honeycomb, Datadog) shows the request natively.
func otelTracing(next http.Handler) http.Handler {
	tracer := obs.Tracer("pandastack-agent/http")
	propagator := otel.GetTextMapPropagator()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		route := normalizeRoute(r.URL.Path)
		spanName := r.Method + " " + route
		ctx, span := tracer.Start(ctx, spanName,
			oteltrace.WithSpanKind(oteltrace.SpanKindServer),
			oteltrace.WithAttributes(
				semconv.HTTPRequestMethodKey.String(r.Method),
				semconv.URLPath(r.URL.Path),
				semconv.HTTPRoute(route),
				attribute.String("pandastack.request_id", RequestIDFrom(ctx)),
				attribute.String("pandastack.workspace", r.Header.Get("X-Fcs-Workspace")),
			),
		)
		defer span.End()

		sr := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(sr, r.WithContext(ctx))

		status := sr.status
		if status == 0 {
			status = 200
		}
		span.SetAttributes(semconv.HTTPResponseStatusCode(status))
		if status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
	})
}
