package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCatalogGatewayFiltersAndRoutesByAuthoritativeCatalog(t *testing.T) {
	providerCalls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls++
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
			if accept := r.Header.Get("Accept"); !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
				http.Error(w, "invalid accept", http.StatusBadRequest)
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
	out := httptest.NewRecorder()
	g.ServeHTTP(out, req)
	if out.Code != 200 || !strings.Contains(out.Body.String(), "orders_search") || !strings.Contains(out.Body.String(), "tenant_claim_list") {
		t.Fatalf("list: %d %s", out.Code, out.Body.String())
	}
	call := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"orders_search","arguments":{}}}`))
	call.Header.Set("Authorization", "Bearer alpha")
	called := httptest.NewRecorder()
	g.ServeHTTP(called, call)
	if called.Code != 200 || providerCalls != 1 {
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
		coreCalls++
		if accept := r.Header.Get("Accept"); !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
			http.Error(w, "invalid accept", http.StatusBadRequest)
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
