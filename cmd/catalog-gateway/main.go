// catalog-gateway is a tenant-aware, stateless MCP aggregation gateway. Tenant
// identity is resolved by tenant-core from the original OAuth bearer; client
// supplied tenantRef headers are never used as catalog authority.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}
type definition struct {
	ServiceName, Class, Endpoint, AuthMode, Health string
	Tools                                          []tool `json:"tools"`
}
type entry struct {
	Revision, State string
	Definition      definition `json:"definition"`
}
type catalog struct {
	TenantRef, Revision string
	Backends            []entry `json:"backends"`
}
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}
type callParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type cachedCatalog struct {
	value   catalog
	expires time.Time
}
type gateway struct {
	core   string
	client *http.Client
	mu     sync.Mutex
	cache  map[string]cachedCatalog
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/livez" {
		w.WriteHeader(200)
		return
	}
	if r.URL.Path != "/mcp" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		rpcHTTPError(w, 401, "OAuth bearer required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rpcHTTPError(w, 400, "invalid JSON-RPC request")
		return
	}
	cat, err := g.catalog(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-RE8CH-Tenant"))
	if err != nil {
		rpcHTTPError(w, 503, err.Error())
		return
	}
	switch req.Method {
	case "initialize":
		rpcResult(w, req.ID, map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{"listChanged": true}}, "serverInfo": map[string]string{"name": "connectool-tenant-gateway", "version": "1.0.0"}})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		var tools []tool
		for _, b := range cat.Backends {
			tools = append(tools, b.Definition.Tools...)
		}
		rpcResult(w, req.ID, map[string]any{"tools": tools, "_meta": map[string]string{"catalogRevision": cat.Revision}})
	case "tools/call":
		var p callParams
		if json.Unmarshal(req.Params, &p) != nil {
			rpcFailure(w, req.ID, -32602, "invalid tool arguments")
			return
		}
		backend, ok := findBackend(cat, p.Name)
		if !ok {
			rpcFailure(w, req.ID, -32601, "tool is not visible to this tenant")
			return
		}
		if backend.State == "paused" || (backend.Definition.Health != "" && backend.Definition.Health != "ready") && isWriteTool(p.Name) {
			rpcFailure(w, req.ID, -32003, "ProviderUnavailable")
			return
		}
		g.forward(w, r, req, backend)
	default:
		rpcFailure(w, req.ID, -32601, "method not found")
	}
}

func (g *gateway) catalog(ctx context.Context, authorization, selectedTenant string) (catalog, error) {
	keyHash := sha256.Sum256([]byte(authorization + "\x00" + selectedTenant))
	key := fmt.Sprintf("%x", keyHash[:])
	g.mu.Lock()
	cached, ok := g.cache[key]
	g.mu.Unlock()
	if ok && time.Now().Before(cached.expires) {
		return cached.value, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(g.core, "/")+"/internal/mcp/catalog", nil)
	if err != nil {
		return catalog{}, err
	}
	req.Header.Set("Authorization", authorization)
	if selectedTenant != "" {
		req.Header.Set("X-RE8CH-Tenant", selectedTenant)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return catalog{}, fmt.Errorf("tenant catalog unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return catalog{}, fmt.Errorf("tenant catalog rejected request: %s", resp.Status)
	}
	var value catalog
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&value); err != nil {
		return catalog{}, err
	}
	if value.TenantRef == "" {
		return catalog{}, errors.New("tenant catalog omitted authority")
	}
	g.mu.Lock()
	g.cache[key] = cachedCatalog{value: value, expires: time.Now().Add(30 * time.Second)}
	g.mu.Unlock()
	return value, nil
}

func findBackend(c catalog, name string) (entry, bool) {
	for _, b := range c.Backends {
		for _, t := range b.Definition.Tools {
			if t.Name == name {
				return b, true
			}
		}
	}
	return entry{}, false
}
func isWriteTool(name string) bool {
	for _, suffix := range []string{"_create", "_update", "_delete", "_bind", "_binding_create", "_rotate", "_renew", "_cancel"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func (g *gateway) forward(w http.ResponseWriter, in *http.Request, rpc rpcRequest, b entry) {
	body, _ := json.Marshal(rpc)
	req, err := http.NewRequestWithContext(in.Context(), http.MethodPost, b.Definition.Endpoint, bytes.NewReader(body))
	if err != nil {
		rpcFailure(w, rpc.ID, -32603, "provider request failed")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if b.Definition.AuthMode == "oauth-forward" {
		req.Header.Set("Authorization", in.Header.Get("Authorization"))
	}
	resp, err := g.client.Do(req)
	if err != nil {
		rpcFailure(w, rpc.ID, -32003, "ProviderUnavailable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rpcFailure(w, rpc.ID, -32003, "ProviderUnavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 4<<20))
}
func rpcResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeRPC(w, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
func rpcFailure(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeRPC(w, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}
func rpcHTTPError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
func writeRPC(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	core := os.Getenv("TENANT_CORE_URL")
	if core == "" {
		log.Fatal("TENANT_CORE_URL is required")
	}
	g := &gateway{core: core, client: &http.Client{Timeout: 60 * time.Second}, cache: map[string]cachedCatalog{}}
	s := &http.Server{Addr: env("LISTEN_ADDR", ":8080"), Handler: g, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(s.ListenAndServe())
}
func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
