// SPDX-License-Identifier: Apache-2.0
// Package obs centralizes observability primitives: Prometheus metrics
// registry/collectors and the OpenTelemetry tracer. Both are designed to
// be cheap when nothing is exporting them (no-op tracer if OTLP endpoint
// is unset, in-memory Prom registry always present).
//
// Env vars:
//
//	OTEL_EXPORTER_OTLP_ENDPOINT   e.g. otel-collector:4318 (HTTP)
//	OTEL_EXPORTER_OTLP_INSECURE   "1" to skip TLS (dev default)
//	OTEL_SERVICE_NAME             override service.name attr (default "pandastack-agent")
package obs

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Reg is the process-wide Prometheus registry.
var Reg = prometheus.NewRegistry()

// ---- Prometheus collectors ------------------------------------------------

var (
	HTTPRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pandastack",
		Name:      "http_requests_total",
		Help:      "Total HTTP requests, partitioned by method and status class.",
	}, []string{"method", "status"})

	HTTPRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "pandastack",
		Name:      "http_request_duration_seconds",
		Help:      "HTTP request duration in seconds.",
		Buckets:   []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"method", "route", "status"})

	SandboxesGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "pandastack",
		Name:      "sandboxes",
		Help:      "Sandboxes currently present, by state.",
	}, []string{"state"})

	SandboxCreatesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pandastack",
		Name:      "sandbox_creates_total",
		Help:      "Total sandbox create attempts, partitioned by result and boot mode.",
	}, []string{"result", "boot_mode"})

	BootDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "pandastack",
		Name:      "sandbox_boot_duration_seconds",
		Help:      "Sandbox boot wall-time (create -> running) in seconds.",
		Buckets:   []float64{.05, .1, .2, .3, .5, .75, 1, 1.5, 2, 3, 5, 10, 30},
	}, []string{"boot_mode"})

	// HibernationTotal counts lifecycle transitions for persistent sandboxes.
	// result=hibernated/woken/wake_failed/hibernate_failed.
	HibernationTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pandastack",
		Name:      "hibernation_total",
		Help:      "Hibernation lifecycle transitions for persistent sandboxes.",
	}, []string{"result"})

	// UffdRestoreTotal counts UFFD streaming-restore attempts (the path gated
	// by PANDASTACK_STREAM_RESTORE=1). result=served when the handler ran to
	// teardown; result=skipped when streaming wasn't set up for the VM.
	UffdRestoreTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pandastack",
		Name:      "uffd_restore_total",
		Help:      "UFFD streaming-restore outcomes, by result.",
	}, []string{"result"})

	// UffdPageFaultsTotal is the cumulative count of guest page faults the
	// resolver answered during streaming restores. Pair with chunk_fetches to
	// gauge how much of the working set the prefetcher warmed ahead of demand.
	UffdPageFaultsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "pandastack",
		Name:      "uffd_page_faults_total",
		Help:      "Total guest page faults served by the UFFD resolver.",
	})

	// UffdChunkFetchesTotal is the cumulative count of chunks actually pulled
	// from the source (file or GCS range) into the local cache. Lower is
	// better relative to faults: it means more faults hit warm cache.
	UffdChunkFetchesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "pandastack",
		Name:      "uffd_chunk_fetches_total",
		Help:      "Total chunks fetched from source into the UFFD cache.",
	})

	// UffdZeroFillTotal is the cumulative count of faults served as zero pages
	// without any I/O (absent chunks in a fresh guest's untouched RAM).
	UffdZeroFillTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "pandastack",
		Name:      "uffd_zero_fill_total",
		Help:      "Total UFFD faults served as zero pages (no I/O).",
	})
)

var registered bool

// RegisterCollectors registers the default pandastack collectors plus the
// go-runtime + process collectors with Reg. Idempotent.
func RegisterCollectors() {
	if registered {
		return
	}
	registered = true
	Reg.MustRegister(
		HTTPRequestsTotal,
		HTTPRequestDuration,
		SandboxesGauge,
		SandboxCreatesTotal,
		BootDuration,
		HibernationTotal,
		UffdRestoreTotal,
		UffdPageFaultsTotal,
		UffdChunkFetchesTotal,
		UffdZeroFillTotal,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// MetricsHandler returns the http.Handler that serves /metrics.
func MetricsHandler() http.Handler {
	RegisterCollectors()
	return promhttp.HandlerFor(Reg, promhttp.HandlerOpts{
		Registry:          Reg,
		EnableOpenMetrics: true,
		Timeout:           5 * time.Second,
	})
}

// ---- OpenTelemetry tracer -------------------------------------------------

var tracerShutdown func(context.Context) error

// InitTracer sets up an OTLP HTTP tracer if OTEL_EXPORTER_OTLP_ENDPOINT is
// configured. Otherwise installs a no-op tracer (Tracer() still works).
// Always installs the W3C trace-context propagator so incoming traceparent
// headers are honored.
func InitTracer(serviceName, version string) (func(context.Context) error, error) {
	if envName := os.Getenv("OTEL_SERVICE_NAME"); envName != "" {
		serviceName = envName
	}
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		otel.SetTracerProvider(sdktrace.NewTracerProvider())
		tracerShutdown = func(ctx context.Context) error { return nil }
		return tracerShutdown, nil
	}

	endpoint = strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(endpoint)}
	if _, err := strconv.ParseBool(os.Getenv("OTEL_EXPORTER_OTLP_INSECURE")); err == nil {
		// any valid bool: respect it
		opts = append(opts, otlptracehttp.WithInsecure())
	} else {
		// default to insecure in dev
		opts = append(opts, otlptracehttp.WithInsecure())
	}

	client := otlptracehttp.NewClient(opts...)
	exp, err := otlptrace.New(context.Background(), client)
	if err != nil {
		return func(ctx context.Context) error { return nil }, err
	}

	res, _ := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(version),
		attribute.String("pandastack.component", serviceName),
	))

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			sdktrace.WithMaxQueueSize(2048),
			sdktrace.WithBatchTimeout(2*time.Second),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio()))),
	)
	otel.SetTracerProvider(tp)

	tracerShutdown = func(ctx context.Context) error {
		_ = tp.ForceFlush(ctx)
		return tp.Shutdown(ctx)
	}
	return tracerShutdown, nil
}

func sampleRatio() float64 {
	if v := os.Getenv("OTEL_TRACES_SAMPLER_RATIO"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 1 {
			return f
		}
	}
	return 1.0
}

// Tracer returns the named tracer from the global provider.
func Tracer(name string) trace.Tracer { return otel.Tracer(name) }
