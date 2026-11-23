package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type invocationBudgetTransport func(*http.Request) (*http.Response, error)

func (f invocationBudgetTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestInvocationPreservesCallerDeadlineAndMetadataBudget(t *testing.T) {
	for _, provider := range []bool{false, true} {
		for _, cancelBusiness := range []bool{false, true} {
			t.Run(fmtBudgetCase(provider, cancelBusiness), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()
				businessCalls, probes, catalogs := 0, 0, 0
				g := &gateway{core: "http://core.test", cache: map[string]cachedCatalog{}}
				g.client = &http.Client{Timeout: time.Minute, Transport: invocationBudgetTransport(func(r *http.Request) (*http.Response, error) {
					deadline, ok := r.Context().Deadline()
					var rpc rpcRequest
					if r.Body != nil {
						if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
							t.Fatal(err)
						}
					}
					business := rpc.Method == "tools/call"
					if !ok || (business && time.Until(deadline) < 9*time.Minute) || (!business && time.Until(deadline) > time.Minute) {
						t.Fatalf("wrong budget business=%v deadline=%v", business, deadline)
					}
					body := `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`
					switch {
					case r.URL.Path == "/internal/mcp/catalog":
						catalogs++
						cat := catalog{TenantRef: "fixture", Revision: "r1"}
						if provider {
							cat.Backends = []entry{{State: "active", Definition: definition{Endpoint: "http://provider.test/mcp", AuthMode: "oauth-forward", Health: "ready", Tools: []tool{{Name: "fixture_read"}}}}}
						}
						raw, _ := json.Marshal(cat)
						body = string(raw)
					case rpc.Method == "tools/list":
						probes++
						body = `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`
					case business:
						businessCalls++
						if (r.URL.Host == "provider.test") != provider {
							t.Fatal("invocation reached the wrong backend")
						}
						if cancelBusiness {
							cancel()
							return nil, r.Context().Err()
						}
					default:
						t.Fatalf("unexpected request %s %s", r.Method, r.URL)
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})}
				req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fixture_read","arguments":{}}}`)).WithContext(ctx)
				req.Header.Set("Authorization", "Bearer fixture")
				out := httptest.NewRecorder()
				g.ServeHTTP(out, req)
				if businessCalls != 1 || probes != 1 || catalogs != 1 {
					t.Fatalf("call replay or metadata workflow changed: business=%d probes=%d catalogs=%d", businessCalls, probes, catalogs)
				}
				if cancelBusiness != strings.Contains(out.Body.String(), "ProviderUnavailable") {
					t.Fatalf("cancellation/error contract changed: %s", out.Body.String())
				}
			})
		}
	}
}

func fmtBudgetCase(provider, canceled bool) string {
	name := "core"
	if provider {
		name = "provider"
	}
	if canceled {
		name += "-canceled"
	}
	return name
}

func TestRealBusinessHTTPOutlivesMetadataClientTimeout(t *testing.T) {
	for _, provider := range []bool{false, true} {
		t.Run(fmtBudgetCase(provider, false), func(t *testing.T) {
			var businessCalls atomic.Int32
			business := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				businessCalls.Add(1)
				time.Sleep(300 * time.Millisecond)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`)
			})
			upstream := httptest.NewServer(business)
			defer upstream.Close()
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/internal/mcp/catalog" {
					cat := catalog{TenantRef: "fixture", Revision: "r1"}
					if provider {
						cat.Backends = []entry{{State: "active", Definition: definition{Endpoint: upstream.URL, Health: "ready", Tools: []tool{{Name: "fixture_read"}}}}}
					}
					json.NewEncoder(w).Encode(cat)
					return
				}
				var rpc rpcRequest
				json.NewDecoder(r.Body).Decode(&rpc)
				if rpc.Method == "tools/list" {
					io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
					return
				}
				business.ServeHTTP(w, r)
			}))
			defer core.Close()
			g := &gateway{core: core.URL, client: &http.Client{Timeout: 100 * time.Millisecond}, cache: map[string]cachedCatalog{}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fixture_read","arguments":{}}}`)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer fixture")
			out := httptest.NewRecorder()
			g.ServeHTTP(out, req)
			if businessCalls.Load() != 1 || strings.Contains(out.Body.String(), "ProviderUnavailable") || !strings.Contains(out.Body.String(), `"result"`) {
				t.Fatalf("business call was truncated or replayed: calls%d %s", businessCalls.Load(), out.Body.String())
			}
		})
	}
}
