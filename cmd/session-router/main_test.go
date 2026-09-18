package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

type staticSource []endpoint

func (s staticSource) Endpoints(context.Context) ([]endpoint, error) { return s, nil }

func TestSessionReturnsToOriginatingEndpoint(t *testing.T) {
	var seenSession string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenSession = r.Header.Get("Mcp-Session-Id")
		w.Header().Set("Mcp-Session-Id", "upstream-session")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	host, portText, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	port, _ := strconv.Atoi(portText)
	handler := &router{source: staticSource{{Address: host, Node: "node-a"}}, nodeName: "node-a", backendPort: port, sessionHeader: "Mcp-Session-Id", transport: http.DefaultTransport}

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "http://router/mcp", nil))
	wrapped := first.Header().Get("Mcp-Session-Id")
	if !strings.HasPrefix(wrapped, tokenPrefix) {
		t.Fatalf("session was not wrapped: %q", wrapped)
	}

	secondReq := httptest.NewRequest(http.MethodPost, "http://router/mcp", nil)
	secondReq.Header.Set("Mcp-Session-Id", wrapped)
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, secondReq)
	if second.Code != http.StatusOK || seenSession != "upstream-session" {
		t.Fatalf("second request status=%d upstream session=%q", second.Code, seenSession)
	}
}

func TestUnknownAndLegacySessionsRequireReinitialize(t *testing.T) {
	handler := &router{source: staticSource{{Address: "10.0.0.1", Node: "node-a"}}, nodeName: "node-a", backendPort: 4483, sessionHeader: "Mcp-Session-Id", transport: http.DefaultTransport}
	for _, session := range []string{"legacy", encodeSession("10.0.0.2", "gone")} {
		req := httptest.NewRequest(http.MethodPost, "http://router/mcp", nil)
		req.Header.Set("Mcp-Session-Id", session)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusNotFound {
			t.Fatalf("session %q returned %d, want 404", session, response.Code)
		}
	}
}

func TestPickerPrefersSameNode(t *testing.T) {
	handler := &router{nodeName: "node-b"}
	chosen := handler.pick([]endpoint{{Address: "10.0.0.1", Node: "node-a"}, {Address: "10.0.0.2", Node: "node-b"}})
	if chosen.Address != "10.0.0.2" {
		t.Fatalf("picked %s, want same-node endpoint", chosen.Address)
	}
}
