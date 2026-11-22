package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDiscoveryRoutingMetadataMatchesBody(t *testing.T) {
	for _, tc := range []struct {
		name, methodHeader, versionHeader, params string
		want                                      int
		version                                   string
	}{
		{"modern omitted headers", "", "", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`, 200, "2026-07-28"},
		{"modern explicit headers", "server/discover", "2026-07-28", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`, 200, "2026-07-28"},
		{"legacy no meta", "", "", `{}`, 200, "2025-03-26"},
		{"method conflict", "tools/list", "2026-07-28", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`, 400, ""},
		{"version conflict", "server/discover", "2025-03-26", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`, 400, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("Mcp-Method") != "server/discover" || r.Header.Get("Mcp-Protocol-Version") != tc.version || r.Header.Get("Authorization") != "Bearer fixture" {
					t.Errorf("routing headers method=%q version=%q", r.Header.Get("Mcp-Method"), r.Header.Get("Mcp-Protocol-Version"))
				}
				var rpc rpcRequest
				json.NewDecoder(r.Body).Decode(&rpc)
				if rpc.Method != "server/discover" || string(rpc.Params) != tc.params {
					t.Error("request body changed")
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"supportedVersions":["2026-07-28"]}}`)
			}))
			defer core.Close()
			g := &gateway{core: core.URL, client: http.DefaultClient, cache: map[string]cachedCatalog{}}
			r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":`+tc.params+`}`))
			r.Header.Set("Authorization", "Bearer fixture")
			if tc.methodHeader != "" {
				r.Header.Set("Mcp-Method", tc.methodHeader)
			}
			if tc.versionHeader != "" {
				r.Header.Set("Mcp-Protocol-Version", tc.versionHeader)
			}
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status%d want%d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want == 400 {
				if requests.Load() != 0 || !strings.Contains(w.Body.String(), `"code":-32020`) {
					t.Fatal("conflicting metadata reached core or lost protocol error")
				}
			} else if requests.Load() != 1 {
				t.Fatal("discovery was replayed")
			}
		})
	}
}

func TestNamedRoutingHeaderUsesTheSameRPCParameters(t *testing.T) {
	for _, tc := range []struct{ method, params, name string }{{"prompts/get", `{"name":"review"}`, "review"}, {"resources/read", `{"uri":"resource://catalog"}`, "resource://catalog"}} {
		var upstream atomic.Int32
		core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstream.Add(1)
			if r.Header.Get("Mcp-Name") != tc.name || r.Header.Get("Mcp-Method") != tc.method {
				t.Error("named request routing metadata diverged")
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		}))
		g := &gateway{core: core.URL, client: http.DefaultClient, cache: map[string]cachedCatalog{}}
		for _, name := range []string{"", "conflict"} {
			r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+tc.method+`","params":`+tc.params+`}`))
			r.Header.Set("Authorization", "Bearer fixture")
			if name != "" {
				r.Header.Set("Mcp-Name", name)
			}
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			want := 200
			if name != "" {
				want = 400
			}
			if w.Code != want {
				t.Fatalf("named routing status%d want%d", w.Code, want)
			}
		}
		if upstream.Load() != 1 {
			t.Fatal("conflicting named request reached core")
		}
		core.Close()
	}
}
