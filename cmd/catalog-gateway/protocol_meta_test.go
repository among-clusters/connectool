package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCallAuthProbePreservesProtocolMetadataWithoutBusinessArguments(t *testing.T) {
	for _, meta := range []string{`{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`, `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","progressToken":"client-progress"}`} {
		t.Run(meta, func(t *testing.T) {
			calls := 0
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/mcp" {
					t.Error("auth denial must never reach catalog")
				}
				if r.Header.Get("Authorization") != "Bearer original" || r.Header.Get("Mcp-Method") != "tools/list" {
					t.Error("probe lost original authentication/read-only method")
				}
				var rpc struct {
					Method string
					Params map[string]json.RawMessage
				}
				_ = json.NewDecoder(r.Body).Decode(&rpc)
				if rpc.Method != "tools/list" || len(rpc.Params) != 1 || string(rpc.Params["_meta"]) != meta {
					t.Errorf("protocol metadata lost or business arguments copied: %s %#v", rpc.Method, rpc.Params)
					http.Error(w, "missing or invalid protocol metadata", 400)
					return
				}
				// Return the core's explicit denial: no catalog/provider may be consulted.
				w.Header().Set("Content-Type", "application/problem+json")
				w.Header().Set("WWW-Authenticate", `Bearer realm="core"`)
				w.WriteHeader(403)
				_, _ = io.WriteString(w, `{"error":"denied"}`)
			}))
			defer core.Close()
			g := &gateway{core: core.URL, client: core.Client(), cache: map[string]cachedCatalog{}}
			r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"never_execute","arguments":{"mustNotBeCopied":true},"_meta":`+meta+`}}`))
			r.Header.Set("Authorization", "Bearer original")
			out := httptest.NewRecorder()
			g.ServeHTTP(out, r)
			if calls != 1 || out.Code != 403 || out.Body.String() != `{"error":"denied"}` || out.Header().Get("WWW-Authenticate") == "" {
				t.Fatalf("probe altered denial or replayed: calls=%d status=%d body=%s", calls, out.Code, out.Body.String())
			}
		})
	}
}
