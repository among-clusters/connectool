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
		w.Write([]byte(`{"tenantRef":"alpha","revision":"r1","backends":[{"revision":"b1","state":"active","definition":{"serviceName":"orders","endpoint":"` + provider.URL + `","authMode":"oauth-forward","health":"ready","tools":[{"name":"orders_search"}]}}]}`))
	}))
	defer core.Close()
	g := &gateway{core: core.URL, client: &http.Client{Timeout: time.Second}, cache: map[string]cachedCatalog{}}
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer alpha")
	out := httptest.NewRecorder()
	g.ServeHTTP(out, req)
	if out.Code != 200 || !strings.Contains(out.Body.String(), "orders_search") {
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
