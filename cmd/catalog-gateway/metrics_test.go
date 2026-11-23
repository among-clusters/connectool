package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRealHTTPMetricsIncludeUnauthenticatedFailureWithoutSensitiveLabels(t *testing.T) {
	t.Setenv("SENTRY_RELEASE", "connectool@fixture")
	t.Setenv("SENTRY_ENVIRONMENT", "test")
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Re8ch-Operation-Id", "op_fixture")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "secretcanary response")
	}))
	defer core.Close()
	g := &gateway{core: core.URL, client: core.Client()}
	var logs bytes.Buffer
	g.telemetry().logger = log.New(&logs, "", 0)
	front := httptest.NewServer(g)
	defer front.Close()
	resp, err := http.Get(front.URL + "/mcp?secretcanary")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatal(resp.StatusCode)
	}
	resp, err = http.Get(front.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatal("not a real Prometheus HTTP surface")
	}
	if !strings.Contains(string(raw), `re8ch_toolchain_http_requests_total{component="connectool-gateway",environment="test",error_category="http_failure",method="GET",outcome="error",phase="gateway_request",status_class="4xx"} 1`) {
		t.Fatalf("missing pre-auth failure counter: %s", raw)
	}
	if !strings.Contains(string(raw), `release="connectool@fixture"`) || !strings.Contains(string(raw), `re8ch_toolchain_build_info`) {
		t.Fatal("missing build identity")
	}
	if strings.Contains(string(raw), "secretcanary") || strings.Contains(string(raw), "op_fixture") || strings.Contains(string(raw), "trace_id") {
		t.Fatal("request data became a metric label")
	}
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &line); err != nil {
		t.Fatal(err)
	}
	if line["operationId"] != "op_fixture" || line["component"] != gatewayComponent || line["release"] != "connectool@fixture" || len(line["traceId"].(string)) != 32 {
		t.Fatal("missing safe log correlation")
	}
	for _, key := range []string{"url", "path", "headers", "payload", "arguments", "user"} {
		if _, ok := line[key]; ok {
			t.Fatalf("unsafe log field %s", key)
		}
	}
	if strings.Contains(logs.String(), "secretcanary") {
		t.Fatal("unsafe request or response log")
	}
	families, err := g.telemetry().registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "re8ch_toolchain_http_requests_total" {
			if len(f.Metric) != 1 || len(f.Metric[0].Label) != 7 {
				t.Fatal("scrape self-counted or label contract changed")
			}
		}
		if f.GetName() == "re8ch_toolchain_http_request_duration_seconds" {
			buckets := f.Metric[0].Histogram.Bucket
			if buckets[len(buckets)-1].GetUpperBound() != 3600 {
				t.Fatal("long-call durations lack finite buckets")
			}
		}
	}
}

type logChannel chan []byte

func (c logChannel) Write(p []byte) (int, error) { c <- append([]byte(nil), p...); return len(p), nil }

func TestMetricsKeepRealStreamFlushCancellationAndSingleInvocation(t *testing.T) {
	stopped := make(chan struct{})
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(stopped)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": keepalive\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer core.Close()
	g := &gateway{core: core.URL, client: &http.Client{Timeout: 20 * time.Millisecond}}
	lines := make(logChannel, 1)
	g.telemetry().logger = log.New(lines, "", 0)
	front := httptest.NewServer(g)
	defer front.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", front.URL+"/mcp", nil)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(": keepalive\n\n"))
	if _, err = io.ReadFull(resp.Body, buf); err != nil || string(buf) != ": keepalive\n\n" {
		t.Fatal("middleware buffered initial stream flush", err)
	}
	// The long-lived request is still active; a metrics scrape must not end it.
	metricResp, err := http.Get(front.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, metricResp.Body)
	metricResp.Body.Close()
	cancel()
	resp.Body.Close()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream survived caller cancellation")
	}
	select {
	case line := <-lines:
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record["outcome"] != "canceled" {
			t.Fatalf("cancellation mislabeled: %s", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("completion not recorded")
	}
	families, _ := g.telemetry().registry.Gather()
	for _, f := range families {
		if f.GetName() == "re8ch_toolchain_http_requests_total" {
			if len(f.Metric) != 1 || f.Metric[0].Counter.GetValue() != 1 {
				t.Fatal("stream replayed or metrics scrape counted")
			}
		}
	}
}

func TestConcurrentMetricsInitializationAndCompletion(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "{}") }))
	defer core.Close()
	g := &gateway{core: core.URL, client: core.Client()}
	front := httptest.NewServer(g)
	defer front.Close()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := http.Get(front.URL + "/mcp")
			if e != nil {
				t.Error(e)
				return
			}
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}()
	}
	wg.Wait()
	families, err := g.telemetry().registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "re8ch_toolchain_http_requests_total" {
			if len(f.Metric) != 1 || f.Metric[0].Counter.GetValue() != 32 {
				t.Fatal("concurrent requests lost or duplicated")
			}
		}
	}
}
