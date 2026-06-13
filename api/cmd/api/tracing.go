// SPDX-License-Identifier: Apache-2.0
package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/pandastack/api/internal/obs"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// mwTracing wraps the inbound handler with a server span and propagates
// the W3C traceparent header into the outgoing reverse-proxy request.
func mwTracing(next http.Handler) http.Handler {
	tracer := obs.Tracer("pandastack-api/http")
	propagator := otel.GetTextMapPropagator()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		route := apiNormalizeRoute(r.URL.Path)
		ctx, span := tracer.Start(ctx, r.Method+" "+route,
			oteltrace.WithSpanKind(oteltrace.SpanKindServer),
			oteltrace.WithAttributes(
				semconv.HTTPRequestMethodKey.String(r.Method),
				semconv.URLPath(r.URL.Path),
				semconv.HTTPRoute(route),
				attribute.String("pandastack.request_id", requestIDFrom(ctx)),
				attribute.String("pandastack.workspace", r.Header.Get("X-Fcs-Workspace")),
			),
		)
		defer span.End()
		// Inject the traceparent into the outgoing headers so the reverse
		// proxy carries it to the agent.
		propagator.Inject(ctx, propagation.HeaderCarrier(r.Header))
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

// mwPromMetrics records request totals + latency histograms.
func mwPromMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(sr, r)
		status := apiStatusClass(sr.status)
		route := apiNormalizeRoute(r.URL.Path)
		obs.HTTPRequestsTotal.WithLabelValues(r.Method, status).Inc()
		obs.HTTPRequestDuration.WithLabelValues(r.Method, route, status).Observe(time.Since(start).Seconds())
	})
}

func apiStatusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code == 0:
		return "2xx"
	}
	return "2xx"
}

// apiNormalizeRoute strips the /v1 prefix and collapses sandbox IDs so
// the route label cardinality is bounded.
func apiNormalizeRoute(p string) string {
	p = strings.TrimPrefix(p, "/v1")
	if p == "" {
		p = "/"
	}
	const pfx = "/sandboxes/"
	if !strings.HasPrefix(p, pfx) {
		return p
	}
	rest := p[len(pfx):]
	if rest == "" {
		return p
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return pfx + "{id}/" + rest[i+1:]
	}
	return pfx + "{id}"
}
