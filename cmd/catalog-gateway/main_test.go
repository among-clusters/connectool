package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCatalogGatewayFiltersAndRoutesByAuthoritativeCatalog(t *testing.T) {
	providerCalls := 0
	coreSessionRequests := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls++
		if r.Header.Get("Mcp-Session-Id") != "" || r.Header.Get("Last-Event-ID") != "" {
			t.Error("core-owned session identifiers leaked to dynamic provider")
			http.Error(w, "unknown provider session", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer alpha" || r.Header.Get("Mcp-Protocol-Version") != "2025-06-18" {
			t.Error("provider lost independent authorization or protocol version")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`))
	}))
	defer provider.Close()
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer alpha" {
			http.Error(w, "no", 401)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/mcp" {
			if r.Header.Get("Mcp-Session-Id") == "core-only-session" {
				coreSessionRequests++
				if r.Header.Get("Last-Event-ID") != "core-only-event" {
					t.Error("core event identifier lost")
				}
			}
			if accept := r.Header.Get("Accept"); !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
				http.Error(w, "invalid accept", http.StatusBadRequest)
				return
			}
			if version := r.Header.Get("Mcp-Protocol-Version"); version != "2025-06-18" {
				http.Error(w, "invalid protocol version", http.StatusBadRequest)
				return
			}
			if method := r.Header.Get("Mcp-Method"); method != "tools/list" {
				http.Error(w, "missing method", http.StatusBadRequest)
				return
			}
			w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"tenant_claim_list"}]}}`))
			return
		}
		w.Write([]byte(`{"tenantRef":"alpha","revision":"r1","backends":[{"revision":"b1","state":"active","definition":{"serviceName":"orders","endpoint":"` + provider.URL + `","authMode":"oauth-forward","health":"ready","tools":[{"name":"orders_search"}]}}]}`))
	}))
	defer core.Close()
	g := &gateway{core: core.URL, client: &http.Client{Timeout: time.Second}, cache: map[string]cachedCatalog{}}
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer alpha")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	req.Header.Set("Mcp-Method", "tools/list")
	out := httptest.NewRecorder()
	g.ServeHTTP(out, req)
	if out.Code != 200 || !strings.Contains(out.Body.String(), "orders_search") || !strings.Contains(out.Body.String(), "tenant_claim_list") {
		t.Fatalf("list: %d %s", out.Code, out.Body.String())
	}
	call := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"orders_search","arguments":{}}}`))
	call.Header.Set("Authorization", "Bearer alpha")
	call.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	call.Header.Set("Mcp-Session-Id", "core-only-session")
	call.Header.Set("Last-Event-ID", "core-only-event")
	called := httptest.NewRecorder()
	g.ServeHTTP(called, call)
	if called.Code != 200 || providerCalls != 1 || coreSessionRequests != 1 {
		t.Fatalf("call: %d calls=%d", called.Code, providerCalls)
	}
}

func TestUnknownProviderToolFallsBackToTenantCore(t *testing.T) {
	coreCalls := 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"tenantRef":"alpha","revision":"empty","backends":[]}`))
			return
		}
		if r.Header.Get("Mcp-Method") == "tools/list" {
			if r.Header.Get("Authorization") != "Bearer alpha" {
				t.Fatal("ping lost bearer")
			}
			w.Write([]byte(`{"jsonrpc":"2.0","id":7,"result":{}}`))
			return
		}
		coreCalls++
		if accept := r.Header.Get("Accept"); !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
			http.Error(w, "invalid accept", http.StatusBadRequest)
			return
		}
		if version := r.Header.Get("Mcp-Protocol-Version"); version != "2025-06-18" {
			http.Error(w, "invalid protocol version", http.StatusBadRequest)
			return
		}
		if method := r.Header.Get("Mcp-Method"); method != "tools/call" {
			http.Error(w, "missing method", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer alpha" || r.Header.Get("X-RE8CH-Tenant") != "alpha" {
			http.Error(w, "missing identity", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"ok"}]}}`))
	}))
	defer core.Close()
	g := &gateway{core: core.URL, client: &http.Client{Timeout: time.Second}, cache: map[string]cachedCatalog{}}
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"tenant_claim_list","arguments":{}}}`))
	req.Header.Set("Authorization", "Bearer alpha")
	req.Header.Set("X-RE8CH-Tenant", "alpha")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	req.Header.Set("Mcp-Method", "tools/call")
	out := httptest.NewRecorder()
	g.ServeHTTP(out, req)
	if out.Code != 200 || coreCalls != 1 || !strings.Contains(out.Body.String(), `"text":"ok"`) {
		t.Fatalf("fallback: %d calls=%d %s", out.Code, coreCalls, out.Body.String())
	}
}

func TestUnhealthyProviderRejectsWritesButKeepsDirectory(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tenantRef":"alpha","revision":"r1","backends":[{"revision":"b1","state":"active","definition":{"serviceName":"orders","endpoint":"https://provider.invalid/mcp","authMode":"oauth-forward","health":"unavailable","tools":[{"name":"orders_create"}]}}]}`))
	}))
	defer core.Close()
	g := &gateway{core: core.URL, client: &http.Client{Timeout: time.Second}, cache: map[string]cachedCatalog{}}
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"orders_create","arguments":{}}}`))
	req.Header.Set("Authorization", "Bearer alpha")
	out := httptest.NewRecorder()
	g.ServeHTTP(out, req)
	if !strings.Contains(out.Body.String(), "ProviderUnavailable") {
		t.Fatalf("unexpected: %s", out.Body.String())
	}
}

func TestInitializeDoesNotDependOnDynamicCatalog(t *testing.T) {
	coreCalls := 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		coreCalls++
		if r.URL.Path != "/mcp" {
			t.Fatal("initialize reached catalog")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"configured-core"}}}`))
	}))
	defer core.Close()
	g := &gateway{core: core.URL, client: core.Client(), cache: map[string]cachedCatalog{}}
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	req.Header.Set("Authorization", "Bearer alpha")
	out := httptest.NewRecorder()
	g.ServeHTTP(out, req)
	if out.Code != 200 || coreCalls != 1 || !strings.Contains(out.Body.String(), "configured-core") {
		t.Fatalf("initialize %d calls=%d %s", out.Code, coreCalls, out.Body.String())
	}
}

func TestToolsListKeepsCoreToolsWhenDynamicCatalogIsUnavailable(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"tenant_claim_list"}]}}`))
	}))
	defer core.Close()
	g := &gateway{core: core.URL, client: &http.Client{Timeout: time.Second}, cache: map[string]cachedCatalog{}}
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer alpha")
	out := httptest.NewRecorder()
	g.ServeHTTP(out, req)
	if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), "tenant_claim_list") {
		t.Fatalf("tools/list: code=%d body=%s", out.Code, out.Body.String())
	}
}

func TestAnonymousMCPPreservesCoreOAuthChallenge(t *testing.T) {
	catalogCalls := 0
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			catalogCalls++
			t.Error("anonymous request reached catalog")
			return
		}
		got, _ := io.ReadAll(r.Body)
		if string(got) != body || r.Header.Get("Authorization") != "" {
			t.Error("anonymous request changed")
		}
		w.Header().Add("WWW-Authenticate", `Bearer resource_metadata="https://core.example/.well-known/oauth-protected-resource/mcp"`)
		w.Header().Add("WWW-Authenticate", `Basic realm="alternate"`)
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("Set-Cookie", "private=hidden")
		w.Header().Set("X-Private-Token", "hidden")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid_token"}`))
	}))
	defer core.Close()
	g := &gateway{core: core.URL, client: core.Client(), cache: map[string]cachedCatalog{}}
	out := httptest.NewRecorder()
	g.ServeHTTP(out, httptest.NewRequest("POST", "/mcp", strings.NewReader(body)))
	challenges := out.Header().Values("WWW-Authenticate")
	if out.Code != 401 || len(challenges) != 2 || challenges[0] != `Bearer resource_metadata="https://core.example/.well-known/oauth-protected-resource/mcp"` || challenges[1] != `Basic realm="alternate"` || out.Header().Get("Content-Type") != "application/problem+json" || out.Body.String() != `{"error":"invalid_token"}` || catalogCalls != 0 {
		t.Fatalf("challenge not preserved: %d %v %s", out.Code, out.Header(), out.Body.String())
	}
	if out.Header().Get("Set-Cookie") != "" || out.Header().Get("X-Private-Token") != "" {
		t.Fatal("private core headers leaked")
	}
}
