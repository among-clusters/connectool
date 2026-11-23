package main

import (
	"context"
	"encoding/json"
	"github.com/getsentry/sentry-go"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type eventSink struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (s *eventSink) Configure(sentry.ClientOptions)        {}
func (s *eventSink) Flush(time.Duration) bool              { return true }
func (s *eventSink) FlushWithContext(context.Context) bool { return true }
func (s *eventSink) Close()                                {}
func (s *eventSink) SendEvent(e *sentry.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}
func TestGatewayTelemetrySafeFailureAndTrace(t *testing.T) {
	sink := &eventSink{}
	client, e := sentry.NewClient(sentry.ClientOptions{Dsn: "https://public@example.invalid/1", Transport: sink, EnableTracing: true, TracesSampleRate: 1, BeforeSend: sanitizeEvent, BeforeSendTransaction: sanitizeEvent})
	if e != nil {
		t.Fatal(e)
	}
	old := sentry.CurrentHub().Client()
	sentry.CurrentHub().BindClient(client)
	defer sentry.CurrentHub().BindClient(old)
	calls := 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !strings.HasPrefix(r.Header.Get("sentry-trace"), "12345678901234567890123456789012-") {
			t.Error("trace not continued")
		}
		if strings.Contains(r.Header.Get("baggage"), "secretcanary") {
			t.Error("incoming baggage leaked")
		}
		w.Header().Set("X-Re8ch-Operation-Id", "op_safe")
		w.WriteHeader(503)
		w.Write([]byte("secretcanary response"))
	}))
	defer core.Close()
	u, _ := url.Parse(core.URL)
	g := &gateway{core: core.URL, client: &http.Client{Transport: tracingTransport{http.DefaultTransport, u}}}
	r := httptest.NewRequest("GET", "http://gateway/mcp?secretcanary", nil)
	r.Header.Set("Authorization", "Bearer secretcanary")
	r.Header.Set("sentry-trace", "12345678901234567890123456789012-1234567890123456-0")
	r.Header.Set("baggage", "sentry-user_id=secretcanary")
	out := httptest.NewRecorder()
	g.ServeHTTP(out, r)
	if calls != 1 || out.Code != 503 || out.Header().Get("X-Re8ch-Operation-Id") != "op_safe" || out.Header().Get("X-Re8ch-Gateway-Request-Id") == "" {
		t.Fatal("response or replay regression")
	}
	errors, traces := 0, 0
	for _, event := range sink.events {
		b, _ := json.Marshal(event)
		if strings.Contains(string(b), "secretcanary") || event.Request != nil {
			t.Fatal("sensitive data in event")
		}
		if event.Type == "transaction" {
			traces++
		} else {
			errors++
			if event.Tags["operation_id"] != "op_safe" {
				t.Fatal("operation missing")
			}
		}
	}
	if errors != 1 || traces != 1 {
		t.Fatalf("events error=%d trace=%d", errors, traces)
	}
}
func TestTracingTransportNeverSendsTraceToProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("sentry-trace") != "" || r.Header.Get("baggage") != "" {
			t.Error("trace leaked to provider")
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	u, _ := url.Parse("http://reviewed-core.invalid")
	r, _ := http.NewRequest("GET", server.URL, nil)
	r.Header.Set("sentry-trace", "untrusted")
	r.Header.Set("baggage", "secretcanary")
	resp, e := (tracingTransport{http.DefaultTransport, u}).RoundTrip(r)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
}

func TestSanitizerRemovesHostAttachmentsAndFrameVariables(t *testing.T) {
	e := &sentry.Event{ServerName: "secretcanary", Attachments: []*sentry.Attachment{{Filename: "secretcanary"}}, Contexts: map[string]sentry.Context{"device": {"name": "secretcanary"}, "trace": {"trace_id": "safe"}}, Exception: []sentry.Exception{{Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{{Function: "safe", Vars: map[string]interface{}{"token": "secretcanary"}}}}}}}
	sanitizeEvent(e, nil)
	b, _ := json.Marshal(e)
	if strings.Contains(string(b), "secretcanary") || len(e.Attachments) != 0 || e.Exception[0].Stacktrace.Frames[0].Function != "safe" {
		t.Fatal("sanitizer removed stack or retained secrets")
	}
}

type panicTransport struct{}

func (panicTransport) RoundTrip(*http.Request) (*http.Response, error) {
	panic("secretcanary panic value")
}
func TestGatewayPanicPreservesSemanticsWithoutPanicValue(t *testing.T) {
	sink := &eventSink{}
	client, e := sentry.NewClient(sentry.ClientOptions{Dsn: "https://public@example.invalid/1", Transport: sink, EnableTracing: true, TracesSampleRate: 1, AttachStacktrace: true, BeforeSend: sanitizeEvent, BeforeSendTransaction: sanitizeEvent})
	if e != nil {
		t.Fatal(e)
	}
	old := sentry.CurrentHub().Client()
	sentry.CurrentHub().BindClient(client)
	defer sentry.CurrentHub().BindClient(old)
	g := &gateway{core: "http://core.invalid", client: &http.Client{Transport: panicTransport{}}}
	func() {
		defer func() {
			if recover() != "secretcanary panic value" {
				t.Error("panic semantics changed")
			}
		}()
		g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://gateway/mcp", nil))
	}()
	failures := 0
	for _, event := range sink.events {
		b, _ := json.Marshal(event)
		if strings.Contains(string(b), "secretcanary") {
			t.Fatal("panic value leaked")
		}
		if event.Type != "transaction" {
			failures++
			if event.Tags["failure_phase"] != "panic" {
				t.Fatal("missing panic phase")
			}
		}
	}
	if failures != 1 {
		t.Fatalf("failures=%d", failures)
	}
}
