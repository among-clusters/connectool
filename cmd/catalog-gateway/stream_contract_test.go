package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNotificationStreamOutlivesJSONTimeoutAndIsNotTruncated(t *testing.T) {
	payload := "data: " + strings.Repeat("x", 5<<20) + "\n\n"
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(60 * time.Millisecond)
		io.WriteString(w, payload)
	}))
	defer core.Close()
	for _, method := range []string{"GET", "POST"} {
		g := &gateway{core: core.URL, client: &http.Client{Timeout: 20 * time.Millisecond}, cache: map[string]cachedCatalog{}}
		front := httptest.NewServer(g)
		body := ""
		if method == "POST" {
			body = `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
		}
		req, _ := http.NewRequest(method, front.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer fixture")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		front.Close()
		if err != nil || resp.StatusCode != 200 || string(raw) != payload {
			t.Fatalf("%s stream ended early: status%d bytes%d err=%v", method, resp.StatusCode, len(raw), err)
		}
	}
}
func TestNotificationStreamHonorsCallerCancellation(t *testing.T) {
	stopped := make(chan struct{})
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(stopped)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer core.Close()
	g := &gateway{core: core.URL, client: &http.Client{Timeout: time.Second}, cache: map[string]cachedCatalog{}}
	front := httptest.NewServer(g)
	defer front.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", front.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer fixture")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("upstream stream survived caller cancellation")
	}
}
