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
	"net/url"
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

func setMCPHeaders(out http.Header, in http.Header) {
	for name, values := range in {
		if !strings.HasPrefix(strings.ToLower(name), "mcp-") {
			continue
		}
		for _, value := range values {
			out.Add(name, value)
		}
	}
	out.Set("Content-Type", "application/json")
	out.Set("Accept", "application/json, text/event-stream")
	version := in.Get("Mcp-Protocol-Version")
	if version == "" {
		version = "2025-03-26"
	}
	out.Set("Mcp-Protocol-Version", version)
}

func (g *gateway) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/livez" {
		w.WriteHeader(200)
		return
	}
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodDelete {
		resp, err := g.coreRequestBody(r.Context(), r, nil)
		g.writeCoreResponse(w, nil, resp, err)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, GET, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		resp, err := g.coreRequestBody(r.Context(), r, http.MaxBytesReader(w, r.Body, 1<<20))
		g.writeCoreResponse(w, nil, resp, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rpcHTTPError(w, 400, "invalid JSON-RPC request")
		return
	}
	if err := normalizeRPCHeaders(r.Header, req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		rpcFailure(w, req.ID, -32020, err.Error())
		return
	}
	switch req.Method {
	case "initialize":
		g.forwardCore(w, r, req)
	case "notifications/initialized":
		g.forwardCore(w, r, req)
	case "tools/list":
		result, rejected, err := g.coreTools(r.Context(), r, req, w)
		if rejected != nil {
			g.writeCoreResponse(w, req.ID, rejected, nil)
			return
		}
		if err != nil {
			rpcHTTPError(w, 503, err.Error())
			return
		}
		cat, err := g.catalog(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-RE8CH-Tenant"))
		if err != nil {
			markFailure(w, "catalog_read")
			log.Printf("dynamic catalog unavailable during tools/list: %v", err)
			cat = catalog{}
		}
		tools, _ := result["tools"].([]any)
		for _, b := range cat.Backends {
			for _, candidate := range b.Definition.Tools {
				tools = append(tools, candidate)
			}
		}
		result["tools"] = tools
		meta, _ := result["_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		meta["catalogRevision"] = cat.Revision
		result["_meta"] = meta
		rpcResult(w, req.ID, result)
	case "tools/call":
		// Revalidate the bearer before catalog/cache access or any provider call.
		// A read-only tools/list never executes the requested tool.
		probe := r.Clone(r.Context())
		probe.Header = r.Header.Clone()
		probe.Header.Set("Mcp-Method", "tools/list")
		// Preserve request protocol metadata without copying tool arguments or name.
		var authParams struct {
			Meta json.RawMessage `json:"_meta,omitempty"`
		}
		_ = json.Unmarshal(req.Params, &authParams)
		probeParams, _ := json.Marshal(authParams)
		resp, err := g.coreRequest(r.Context(), probe, rpcRequest{JSONRPC: "2.0", ID: req.ID, Method: "tools/list", Params: probeParams})
		if err != nil {
			rpcHTTPError(w, 503, "tenant core unavailable")
			return
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			g.writeCoreResponse(w, req.ID, resp, nil)
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		var p callParams
		if json.Unmarshal(req.Params, &p) != nil {
			rpcFailure(w, req.ID, -32602, "invalid tool arguments")
			return
		}
		cat, err := g.catalog(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-RE8CH-Tenant"))
		if err != nil {
			markFailure(w, "catalog_read")
			log.Printf("dynamic catalog unavailable during tools/call: %v", err)
			g.forwardCore(w, r, req)
			return
		}
		backend, ok := findBackend(cat, p.Name)
		if !ok {
			g.forwardCore(w, r, req)
			return
		}
		if backend.State == "paused" || (backend.Definition.Health != "" && backend.Definition.Health != "ready") && isWriteTool(p.Name) {
			rpcFailure(w, req.ID, -32003, "ProviderUnavailable")
			return
		}
		g.forward(w, r, req, backend)
	default:
		g.forwardCore(w, r, req)
	}
}

// Routing metadata and JSON-RPC describe one request. Fill omitted headers from
// its body, while refusing contradictory caller declarations rather than
// silently selecting a different method or protocol version.
func normalizeRPCHeaders(header http.Header, rpc rpcRequest) error {
	if method := header.Get("Mcp-Method"); method != "" && method != rpc.Method {
		return errors.New("Mcp-Method conflicts with JSON-RPC method")
	}
	header.Set("Mcp-Method", rpc.Method)
	var params struct {
		Meta map[string]json.RawMessage `json:"_meta"`
		Name string                     `json:"name"`
		URI  string                     `json:"uri"`
	}
	if len(rpc.Params) == 0 || json.Unmarshal(rpc.Params, &params) != nil {
		return nil // Existing core/provider argument validation remains authoritative.
	}
	if raw := params.Meta["io.modelcontextprotocol/protocolVersion"]; len(raw) != 0 {
		var version string
		if json.Unmarshal(raw, &version) != nil || version == "" {
			return errors.New("invalid protocolVersion metadata")
		}
		if declared := header.Get("Mcp-Protocol-Version"); declared != "" && declared != version {
			return errors.New("Mcp-Protocol-Version conflicts with protocolVersion metadata")
		}
		header.Set("Mcp-Protocol-Version", version)
	}
	name := ""
	switch rpc.Method {
	case "tools/call", "prompts/get":
		name = params.Name
	case "resources/read":
		name = params.URI
	}
	if name != "" {
		if declared := header.Get("Mcp-Name"); declared != "" && declared != name {
			return errors.New("Mcp-Name conflicts with request parameters")
		}
		header.Set("Mcp-Name", name)
	}
	return nil
}

func (g *gateway) coreRequest(ctx context.Context, in *http.Request, rpc rpcRequest) (*http.Response, error) {
	body, err := json.Marshal(rpc)
	if err != nil {
		return nil, err
	}
	return g.coreRequestBody(ctx, in, bytes.NewReader(body))
}

func (g *gateway) coreRequestBody(ctx context.Context, in *http.Request, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, in.Method, strings.TrimSuffix(g.core, "/")+"/mcp", body)
	if err != nil {
		return nil, err
	}
	setMCPHeaders(req.Header, in.Header)
	if eventID := in.Header.Get("Last-Event-ID"); eventID != "" {
		req.Header.Set("Last-Event-ID", eventID)
	}
	// Streamable HTTP MCP servers require clients to advertise both response
	// representations, even when this gateway ultimately returns JSON to its
	// caller.  Normalize the internal hop so older JSON-only MCP clients remain
	// compatible with a spec-compliant tenant core.
	req.Header.Set("Authorization", in.Header.Get("Authorization"))
	if selected := in.Header.Get("X-RE8CH-Tenant"); selected != "" {
		req.Header.Set("X-RE8CH-Tenant", selected)
	}
	client := g.client
	if in.Method == http.MethodGet || in.Header.Get("Mcp-Method") == "subscriptions/listen" || in.Header.Get("Mcp-Method") == "tools/call" {
		client = g.callerBoundClient()
	}
	return client.Do(req)
}

func (g *gateway) callerBoundClient() *http.Client {
	// Tool execution and notification streams inherit cancellation/deadlines
	// from the original caller. Keep the separate client budget for discovery,
	// authorization probes and catalog reads, without replaying a business call.
	client := *g.client
	client.Timeout = 0
	return &client
}

func (g *gateway) coreTools(ctx context.Context, in *http.Request, rpc rpcRequest, w http.ResponseWriter) (map[string]any, *http.Response, error) {
	resp, err := g.coreRequest(ctx, in, rpc)
	if err != nil {
		return nil, nil, fmt.Errorf("tenant core unavailable: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp, nil
	}
	defer resp.Body.Close()
	var envelope struct {
		Result map[string]any `json:"result"`
		Error  any            `json:"error"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope); err != nil {
		return nil, nil, fmt.Errorf("tenant core returned invalid tools/list: %w", err)
	}
	if envelope.Error != nil || envelope.Result == nil {
		return nil, nil, errors.New("tenant core rejected tools/list")
	}
	for _, name := range []string{"Mcp-Session-Id", "Mcp-Protocol-Version", "X-Re8ch-Operation-Id"} {
		for _, value := range resp.Header.Values(name) {
			w.Header().Add(name, value)
		}
	}
	return envelope.Result, nil, nil
}

func (g *gateway) forwardCore(w http.ResponseWriter, in *http.Request, rpc rpcRequest) {
	resp, err := g.coreRequest(in.Context(), in, rpc)
	g.writeCoreResponse(w, rpc.ID, resp, err)
}

func (g *gateway) writeCoreResponse(w http.ResponseWriter, id json.RawMessage, resp *http.Response, err error) {
	if err != nil {
		markFailure(w, "core_transport")
		rpcFailure(w, id, -32003, "ProviderUnavailable")
		return
	}
	defer resp.Body.Close()
	for _, name := range []string{"Content-Type", "WWW-Authenticate", "Mcp-Session-Id", "Mcp-Protocol-Version", "Cache-Control", "Allow", "X-Re8ch-Operation-Id"} {
		for _, value := range resp.Header.Values(name) {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		controller := http.NewResponseController(w)
		_ = controller.Flush()
		if _, err := io.Copy(flushingWriter{writer: w, controller: controller}, resp.Body); err != nil {
			markFailure(w, "response_stream")
		}
		return
	}
	if _, err := io.Copy(w, io.LimitReader(resp.Body, 4<<20)); err != nil {
		markFailure(w, "response_stream")
	}
}

type flushingWriter struct {
	writer     io.Writer
	controller *http.ResponseController
}

func (w flushingWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err == nil {
		err = w.controller.Flush()
	}
	return n, err
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
	setMCPHeaders(req.Header, in.Header)
	// Session and event IDs belong to the configured core, not this provider.
	req.Header.Del("Mcp-Session-Id")
	req.Header.Del("Last-Event-ID")
	if b.Definition.AuthMode == "oauth-forward" {
		req.Header.Set("Authorization", in.Header.Get("Authorization"))
	}
	resp, err := g.callerBoundClient().Do(req)
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
	if _, err := io.Copy(w, io.LimitReader(resp.Body, 4<<20)); err != nil {
		markFailure(w, "response_stream")
	}
}
func rpcResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeRPC(w, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
func rpcFailure(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	markFailure(w, "rpc_failure")
	writeRPC(w, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}
func rpcHTTPError(w http.ResponseWriter, status int, message string) {
	markFailure(w, "http_failure")
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
	if err := initTelemetry(); err != nil {
		log.Print("gateway diagnostics Sentry configuration invalid; local request identifiers remain enabled")
	}
	coreURL, err := url.Parse(core)
	if err != nil {
		log.Fatal("invalid Core configuration")
	}
	g := &gateway{core: core, client: &http.Client{Timeout: 60 * time.Second, Transport: tracingTransport{base: http.DefaultTransport, core: coreURL}}, cache: map[string]cachedCatalog{}}
	s := &http.Server{Addr: env("LISTEN_ADDR", ":8080"), Handler: g, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(s.ListenAndServe())
}
func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
