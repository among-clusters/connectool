package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/getsentry/sentry-go"
)

// No payload, caller identity, URL, header map, or original error is attached.
func initTelemetry() error {
	return sentry.Init(sentry.ClientOptions{Dsn: os.Getenv("SENTRY_DSN"), Release: os.Getenv("SENTRY_RELEASE"), Environment: os.Getenv("SENTRY_ENVIRONMENT"), EnableTracing: true, TracesSampleRate: 1, SampleRate: 1, SendDefaultPII: false, AttachStacktrace: true,
		BeforeSend: sanitizeEvent, BeforeSendTransaction: sanitizeEvent})
}
func sanitizeEvent(e *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	e.Request = nil
	e.User = sentry.User{}
	e.Breadcrumbs = nil
	e.Modules = nil
	e.ServerName = ""
	e.Attachments = nil
	for key := range e.Contexts {
		if key != "trace" {
			delete(e.Contexts, key)
		}
	}
	for i := range e.Exception {
		if st := e.Exception[i].Stacktrace; st != nil {
			for j := range st.Frames {
				st.Frames[j].Vars = nil
			}
		}
	}
	for _, thread := range e.Threads {
		if thread.Stacktrace != nil {
			for j := range thread.Stacktrace.Frames {
				thread.Stacktrace.Frames[j].Vars = nil
			}
		}
	}
	// Only explicitly constructed diagnostics are retained. Never collect SDK request data.
	return e
}

type gatewayRequestIDKey struct{}

type diagnosticWriter struct {
	http.ResponseWriter
	status  int
	failure string
}

func (w *diagnosticWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *diagnosticWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *diagnosticWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	n, e := w.ResponseWriter.Write(p)
	if e != nil {
		w.failure = "response_write"
	}
	return n, e
}
func (w *diagnosticWriter) fail(kind string) {
	if w.failure == "" {
		w.failure = kind
	}
}
func markFailure(w http.ResponseWriter, kind string) {
	if d, ok := w.(*diagnosticWriter); ok {
		d.fail(kind)
	}
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/livez" {
		g.serveHTTP(w, r)
		return
	}
	hub := sentry.CurrentHub().Clone()
	hub.ConfigureScope(func(s *sentry.Scope) { s.Clear(); s.SetTag("component", "connectool-gateway") })
	ctx := sentry.SetHubOnContext(r.Context(), hub)
	// Incoming baggage is untrusted and may contain arbitrary caller identity.
	span := sentry.StartTransaction(ctx, "mcp.gateway", sentry.ContinueFromHeaders(r.Header.Get("sentry-trace"), ""), sentry.WithOpName("http.server"), sentry.WithSpanSampled(sentry.SampledTrue))
	r = r.WithContext(span.Context())
	var id [16]byte
	_, _ = rand.Read(id[:])
	callID := hex.EncodeToString(id[:])
	r = r.WithContext(context.WithValue(r.Context(), gatewayRequestIDKey{}, callID))
	hub.Scope().SetTag("gateway_request_id", callID)
	span.SetTag("gateway_request_id", callID)
	w.Header().Set("X-Re8ch-Gateway-Request-Id", callID)
	observed := &diagnosticWriter{ResponseWriter: w}
	start := time.Now()
	defer func() {
		panicValue := recover()
		if panicValue != nil {
			observed.failure = "panic"
		}
		if observed.status >= 400 {
			observed.fail("http_failure")
		}
		if r.Context().Err() != nil {
			observed.fail("caller_canceled")
		}
		span.SetData("duration_ms", time.Since(start).Milliseconds())
		span.SetData("http_status", observed.status)
		if op := observed.Header().Get("X-Re8ch-Operation-Id"); op != "" {
			hub.Scope().SetTag("operation_id", op)
			span.SetTag("operation_id", op)
		}
		if observed.failure != "" {
			span.Status = sentry.SpanStatusInternalError
			hub.Scope().SetTag("failure_phase", observed.failure)
			hub.Scope().SetLevel(sentry.LevelError)
			hub.CaptureMessage("MCP gateway request failed")
		} else {
			span.Status = sentry.SpanStatusOK
		}
		span.Finish() // SDK transport is asynchronous; no Flush on request path.
		if panicValue != nil {
			panic(panicValue)
		}
	}()
	g.serveHTTP(observed, r)
}

// Propagate only to the reviewed Core origin, never to catalog-selected providers.
type tracingTransport struct {
	base http.RoundTripper
	core *url.URL
}

func (t tracingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	clone.Header.Del("sentry-trace")
	clone.Header.Del("baggage")
	if t.core != nil && r.URL.Scheme == t.core.Scheme && r.URL.Host == t.core.Host {
		if id, ok := r.Context().Value(gatewayRequestIDKey{}).(string); ok {
			clone.Header.Set("X-Re8ch-Gateway-Request-Id", id)
		}
		if span := sentry.SpanFromContext(r.Context()); span != nil {
			clone.Header.Set("sentry-trace", span.ToSentryTrace())
			clone.Header.Set("baggage", span.ToBaggage())
		}
	}
	return t.base.RoundTrip(clone)
}
