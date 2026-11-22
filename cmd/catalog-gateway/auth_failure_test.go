package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestInvalidBearerPreservesCoreAuthFailures(t *testing.T) {
	for _, method := range []string{"tools/list", "tools/call", "initialize", "notifications/initialized"} {
		for _, status := range []int{401, 403} {
			t.Run(fmt.Sprintf("%s/%d", method, status), func(t *testing.T) {
				calls, catalog := 0, 0
				challenges := []string{`Bearer realm="core", resource_metadata="https://core.example/resource"`, `Basic realm="other"`}
				core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.URL.Path != "/mcp" {
						catalog++
						t.Error("invalid bearer reached catalog")
					}
					if r.Header.Get("Authorization") != "Bearer invalid" {
						t.Error("bearer changed")
					}
					var rpc rpcRequest
					if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
						t.Error(err)
					}
					if method == "tools/call" && rpc.Method != "tools/list" {
						t.Error("unvalidated business call executed")
					}
					if status == 401 {
						for _, h := range challenges {
							w.Header().Add("WWW-Authenticate", h)
						}
					}
					w.Header().Set("Content-Type", "application/problem+json")
					w.Header().Set("X-Private", "hidden")
					w.WriteHeader(status)
					w.Write([]byte(`{"error":"denied"}`))
				}))
				defer core.Close()
				g := &gateway{core: core.URL, client: core.Client(), cache: map[string]cachedCatalog{}}
				req := httptest.NewRequest("POST", "/mcp", strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":{"name":"some_tool","arguments":{}}}`, method)))
				req.Header.Set("Authorization", "Bearer invalid")
				out := httptest.NewRecorder()
				g.ServeHTTP(out, req)
				expected := challenges
				if status == 403 {
					expected = nil
				}
				if out.Code != status || out.Body.String() != `{"error":"denied"}` || out.Header().Get("Content-Type") != "application/problem+json" || !reflect.DeepEqual(out.Header().Values("WWW-Authenticate"), expected) || calls != 1 || catalog != 0 || out.Header().Get("X-Private") != "" {
					t.Fatalf("status=%d headers=%v body=%s calls=%d catalog=%d", out.Code, out.Header(), out.Body.String(), calls, catalog)
				}
			})
		}
	}
}

type authRoundTripper func(*http.Request) (*http.Response, error)

func (f authRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }

func TestAuthFailureBodyIsBoundedAndClosed(t *testing.T) {
	for _, method := range []string{"tools/list", "tools/call", "initialize", "notifications/initialized"} {
		t.Run(method, func(t *testing.T) {
			body := &trackedBody{Reader: strings.NewReader(strings.Repeat("x", (4<<20)+100))}
			calls := 0
			g := &gateway{core: "https://core.invalid", client: &http.Client{Transport: authRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 401, Header: http.Header{"Www-Authenticate": []string{`Bearer realm="core"`}}, Body: body}, nil
			})}, cache: map[string]cachedCatalog{}}
			req := httptest.NewRequest("POST", "/mcp", strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q}`, method)))
			req.Header.Set("Authorization", "Bearer invalid")
			out := httptest.NewRecorder()
			g.ServeHTTP(out, req)
			if !body.closed || calls != 1 || out.Code != 401 || out.Body.Len() != 4<<20 {
				t.Fatalf("closed=%v calls=%d code=%d bytes=%d", body.closed, calls, out.Code, out.Body.Len())
			}
		})
	}
}

func TestCoreTransportFailureRemainsUnavailable(t *testing.T) {
	for _, method := range []string{"tools/list", "tools/call"} {
		t.Run(method, func(t *testing.T) {
			calls := 0
			g := &gateway{core: "https://core.invalid", client: &http.Client{Transport: authRoundTripper(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("offline") })}, cache: map[string]cachedCatalog{}}
			req := httptest.NewRequest("POST", "/mcp", strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q}`, method)))
			req.Header.Set("Authorization", "Bearer invalid")
			out := httptest.NewRecorder()
			g.ServeHTTP(out, req)
			if out.Code != 503 || calls != 1 || out.Header().Get("WWW-Authenticate") != "" {
				t.Fatalf("code=%d calls=%d", out.Code, calls)
			}
		})
	}
}

func TestInitializedNotificationUsesCoreAuthentication(t *testing.T) {
	calls := 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/mcp" || r.Header.Get("Authorization") != "Bearer valid" {
			t.Error("notification lost authority")
		}
		var rpc rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil || rpc.Method != "notifications/initialized" {
			t.Error("notification changed")
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer core.Close()
	g := &gateway{core: core.URL, client: core.Client(), cache: map[string]cachedCatalog{}}
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("Authorization", "Bearer valid")
	out := httptest.NewRecorder()
	g.ServeHTTP(out, req)
	if out.Code != 202 || calls != 1 || out.Body.Len() != 0 {
		t.Fatalf("code=%d calls=%d body=%s", out.Code, calls, out.Body.String())
	}
}
