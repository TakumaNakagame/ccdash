// Package tracing is ccdash's OpenTelemetry wiring. It is off unless the
// standard OTLP environment says where to send spans
// (OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT,
// OTLP/HTTP only): then Start installs a TracerProvider and the W3C
// propagators. Off, the global provider stays the no-op one, so the
// otelhttp wrappers and Span calls cost next to nothing and no trace
// header is read or written — the same behaviour as before tracing.
//
// High-frequency paths (portal and TUI polling, statusline, per-tool hooks,
// long-lived streams) never get a span: see Traced.
package tracing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/TakumaNakagame/ccdash/internal/buildinfo"
)

const scope = "github.com/TakumaNakagame/ccdash"

// Enabled reports whether the environment asks for traces.
func Enabled() bool {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return false
	}
	if strings.EqualFold(os.Getenv("OTEL_TRACES_EXPORTER"), "none") {
		return false
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

// Start installs the global TracerProvider when Enabled, naming the
// service service unless OTEL_SERVICE_NAME says otherwise. The returned
// func flushes and stops it (a no-op when tracing is off); call it before
// the process exits or re-execs.
func Start(ctx context.Context, service string) (func(), error) {
	if !Enabled() {
		return func() {}, nil
	}
	if p := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"); p == "" {
		if p = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"); p != "" && p != "http/protobuf" {
			log.Printf("tracing: OTEL_EXPORTER_OTLP_PROTOCOL=%s is not supported; sending OTLP/HTTP (http/protobuf)", p)
		}
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return func() {}, fmt.Errorf("tracing: exporter: %w", err)
	}
	// Later detectors win: OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES
	// override the default name. (Not os.Setenv: the hosted claudes
	// inherit this environment.)
	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", service),
			attribute.String("service.version", buildinfo.Version)),
		resource.WithFromEnv(),
		resource.WithHost(),
		resource.WithTelemetrySDK(),
	)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return func() {}, fmt.Errorf("tracing: resource: %w", err)
	}
	// The sampler comes from OTEL_TRACES_SAMPLER (default: parent-based
	// always-on, so the ingress's decision carries through).
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Printf("tracing: %v", err)
	}))
	name := service
	if v, ok := res.Set().Value("service.name"); ok {
		name = v.AsString()
	}
	log.Printf("tracing: exporting spans as %s", name)
	return func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(sctx); err != nil {
			log.Printf("tracing: shutdown: %v", err)
		}
	}, nil
}

// Span starts an internal span for a slow operation (a session start, a
// restart, title generation). End it with the returned func, passing the
// operation's error (nil = ok).
func Span(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, func(error)) {
	ctx, sp := otel.Tracer(scope).Start(ctx, name, trace.WithAttributes(attrs...))
	return ctx, func(err error) {
		if err != nil {
			sp.RecordError(err)
			sp.SetStatus(codes.Error, err.Error())
		}
		sp.End()
	}
}

// Detach keeps ctx's span as the parent of work that outlives the request
// (a background goroutine) without inheriting its cancellation.
func Detach(ctx context.Context) context.Context {
	return trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(ctx))
}

// Handler wraps a server's handler with a span per traced request.
func Handler(h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "http",
		otelhttp.WithFilter(Traced),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return SpanName(r) }),
	)
}

// Transport wraps a client transport so traced requests get a client span
// and carry traceparent to the other side.
func Transport(rt http.RoundTripper) http.RoundTripper {
	return otelhttp.NewTransport(rt,
		otelhttp.WithFilter(Traced),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return SpanName(r) }),
	)
}

// SpanName is "METHOD /route", with id-like path segments replaced so
// names stay low-cardinality ("/api/d/{id}/pty/{id}/input").
func SpanName(r *http.Request) string {
	return r.Method + " " + Route(r.URL.Path)
}

// Route replaces the id-like segments of path with {id}.
func Route(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if idLike(s) {
			segs[i] = "{id}"
		}
	}
	return strings.Join(segs, "/")
}

// idLike: a session id / pty key / device id / approval number — anything
// with a digit that isn't a short word ("v1"), or a pid-N key.
func idLike(s string) bool {
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "pid-") {
		return true
	}
	digits := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			digits++
		}
	}
	return digits == len(s) || (digits > 0 && len(s) >= 6)
}

// skipped are paths too frequent to trace: statusline (every UI update of
// every claude), per-tool hooks, read marks, PTY resizes.
var skipped = []string{
	"/hooks/statusline", "/hooks/pre-tool", "/hooks/post-tool",
	"/seen", "/resize",
}

// tracedGets are the GETs worth a span; every other GET (and HEAD) is
// polling — sessions, board, devices, me, transcript tails, PTY lists and
// screens — or a long-lived stream, and is never traced.
var tracedGets = []string{
	"/auth/", "/s/", "/api/locate/", "/hub/git/", "/hub/file", "/api/usage",
}

// Traced decides whether a request gets a span (server and client side).
func Traced(r *http.Request) bool {
	p := r.URL.Path
	for _, s := range skipped {
		if strings.HasSuffix(p, s) || strings.HasPrefix(p, s) {
			return false
		}
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		if r.Header.Get("Upgrade") != "" {
			return false
		}
		for _, g := range tracedGets {
			// /api/d/<device>/… is the hub's relay of the device path.
			if strings.HasPrefix(p, g) || (strings.HasPrefix(p, "/api/d/") && strings.Contains(p, g)) {
				return true
			}
		}
		return false
	}
	return true
}
