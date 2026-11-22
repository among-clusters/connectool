package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSessionMethodsPreserveCoreLifecycleAndAuthentication(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		for _, status := range []int{200, 204, 401, 403, 404, 405} {
			t.Run(fmt.Sprintf("%s/%d", method, status), func(t *testing.T) {
				calls := 0
				core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.URL.Path != "/mcp" || r.Method != method || r.Header.Get("Authorization") != "Bearer client-only" || r.Header.Get("Mcp-Session-Id") != "session-only" || r.Header.Get("Last-Event-ID") != "event-only" || r.Header.Get("Mcp-Protocol-Version") != "2025-06-18" {
						t.Errorf("protocol request was changed: %s %s %v", r.Method, r.URL.Path, r.Header)
					}
					w.Header().Set("Content-Type", "application/problem+json")
					w.Header().Set("Mcp-Session-Id", "session-only")
					w.Header().Set("Mcp-Protocol-Version", "2025-06-18")
					w.Header().Add("WWW-Authenticate", `Bearer resource_metadata="https://core.example/metadata"`)
					w.Header().Add("WWW-Authenticate", `Basic realm="alternate"`)
					w.Header().Set("Allow", "POST, GET, DELETE")
					w.Header().Set("X-Private-Core", "must-not-leak")
					w.WriteHeader(status)
					if status != 204 {
						_, _ = io.WriteString(w, `{"outcome":"from-core"}`)
					}
				}))
				defer core.Close()
				g := &gateway{core: core.URL, client: core.Client(), cache: map[string]cachedCatalog{}}
				request := httptest.NewRequest(method, "/mcp", nil)
				request.Header.Set("Authorization", "Bearer client-only")
				request.Header.Set("Mcp-Session-Id", "session-only")
				request.Header.Set("Mcp-Protocol-Version", "2025-06-18")
				request.Header.Set("Last-Event-ID", "event-only")
				recorder := httptest.NewRecorder()
				g.ServeHTTP(recorder, request)
				if recorder.Code != status || calls != 1 || recorder.Header().Get("Mcp-Session-Id") != "session-only" || recorder.Header().Get("Mcp-Protocol-Version") != "2025-06-18" || len(recorder.Header().Values("WWW-Authenticate")) != 2 || recorder.Header().Get("Allow") != "POST, GET, DELETE" || recorder.Header().Get("X-Private-Core") != "" {
					t.Fatalf("lifecycle response mismatch status=%d calls=%d headers=%v", recorder.Code, calls, recorder.Header())
				}
				if status != 204 && recorder.Body.String() != `{"outcome":"from-core"}` {
					t.Fatal("core body changed")
				}
			})
		}
	}
}

func TestInitializeSessionAndUnknownMethodsForwardWithoutCatalogOrReplay(t *testing.T) {
	for _, method := range []string{"initialize", "notifications/cancelled", "ping"} {
		t.Run(method, func(t *testing.T) {
			calls := 0
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/mcp" || r.Method != "POST" {
					t.Error("unexpected catalog access")
				}
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), `"method":"`+method+`"`) {
					t.Error("method changed")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Mcp-Session-Id", "new-session")
				w.WriteHeader(200)
				_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			}))
			defer core.Close()
			g := &gateway{core: core.URL, client: core.Client(), cache: map[string]cachedCatalog{}}
			r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+method+`"}`))
			r.Header.Set("Authorization", "Bearer original")
			out := httptest.NewRecorder()
			g.ServeHTTP(out, r)
			if calls != 1 || out.Header().Get("Mcp-Session-Id") != "new-session" || out.Code != 200 {
				t.Fatalf("session lost or request replayed: %d %v", calls, out.Header())
			}
		})
	}
}

func TestSessionGETStreamsBeforeCoreClosesAndCancelsUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Mcp-Session-Id", "session")
		_, _ = io.WriteString(w, "id: one\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer core.Close()
	g := &gateway{core: core.URL, client: core.Client(), cache: map[string]cachedCatalog{}}
	gatewayServer := httptest.NewServer(g)
	defer gatewayServer.Close()
	client := &http.Client{Timeout: 2 * time.Second}
	r, _ := http.NewRequest("GET", gatewayServer.URL+"/mcp", nil)
	r.Header.Set("Authorization", "Bearer client")
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "id: one\n" {
		t.Fatalf("event buffered until upstream EOF: %q %v", line, err)
	}
	if resp.Header.Get("Mcp-Session-Id") != "session" || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatal("stream headers lost")
	}
	_ = resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("client disconnect did not release core stream")
	}
}

func TestAnonymousSessionMethodsReachCoreAuthentication(t *testing.T) {
	for _, method := range []string{"GET", "DELETE"} {
		calls := 0
		core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.Method != method || r.Header.Get("Authorization") != "" {
				t.Error("anonymous method changed")
			}
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://core.example/metadata"`)
			w.WriteHeader(401)
		}))
		g := &gateway{core: core.URL, client: core.Client(), cache: map[string]cachedCatalog{}}
		out := httptest.NewRecorder()
		g.ServeHTTP(out, httptest.NewRequest(method, "/mcp", nil))
		core.Close()
		if calls != 1 || out.Code != 401 || out.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("anonymous %s bypassed core auth", method)
		}
	}
}

func TestExpiredSessionIsNotReclassifiedOrReplayed(t *testing.T) {
	for _, method := range []string{"tools/list", "tools/call"} {
		t.Run(method, func(t *testing.T) {
			calls := 0
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/mcp" {
					t.Error("expired session reached catalog")
				}
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(404)
				_, _ = io.WriteString(w, "session not found")
			}))
			defer core.Close()
			g := &gateway{core: core.URL, client: core.Client(), cache: map[string]cachedCatalog{}}
			r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":{"name":"never_execute","arguments":{}}}`))
			r.Header.Set("Authorization", "Bearer valid")
			r.Header.Set("Mcp-Session-Id", "expired")
			out := httptest.NewRecorder()
			g.ServeHTTP(out, r)
			if calls != 1 || out.Code != 404 || out.Body.String() != "session not found" || out.Header().Get("Content-Type") != "text/plain" {
				t.Fatalf("session 404 hidden or request replayed: %d %d %q", calls, out.Code, out.Body.String())
			}
		})
	}
}
