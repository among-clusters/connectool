// session-router keeps stateful Streamable HTTP MCP sessions on the vMCP pod
// that created them without introducing a shared session database.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const tokenPrefix = "ct1."

type endpoint struct {
	Address string
	Node    string
}

type endpointSource interface {
	Endpoints(context.Context) ([]endpoint, error)
}

type kubeEndpointSource struct {
	client   *http.Client
	url      string
	token    string
	cacheTTL time.Duration
	mu       sync.Mutex
	cached   []endpoint
	cachedAt time.Time
}

type endpointSliceList struct {
	Items []struct {
		Endpoints []struct {
			Addresses  []string `json:"addresses"`
			NodeName   string   `json:"nodeName"`
			Conditions struct {
				Ready       *bool `json:"ready"`
				Terminating *bool `json:"terminating"`
			} `json:"conditions"`
		} `json:"endpoints"`
	} `json:"items"`
}

func (s *kubeEndpointSource) Endpoints(ctx context.Context) ([]endpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cached) > 0 && time.Since(s.cachedAt) < s.cacheTTL {
		return append([]endpoint(nil), s.cached...), nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("EndpointSlice API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var list endpointSliceList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	var result []endpoint
	for _, slice := range list.Items {
		for _, item := range slice.Endpoints {
			if (item.Conditions.Ready != nil && !*item.Conditions.Ready) ||
				(item.Conditions.Terminating != nil && *item.Conditions.Terminating) {
				continue
			}
			for _, address := range item.Addresses {
				result = append(result, endpoint{Address: address, Node: item.NodeName})
			}
		}
	}
	if len(result) == 0 {
		return nil, errors.New("no ready vMCP endpoints")
	}
	s.cached, s.cachedAt = result, time.Now()
	return append([]endpoint(nil), result...), nil
}

type router struct {
	source        endpointSource
	transport     http.RoundTripper
	nodeName      string
	backendPort   int
	sessionHeader string
	next          atomic.Uint64
}

func (r *router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/livez" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	if req.URL.Path == "/healthz" {
		if endpoints, err := r.source.Endpoints(req.Context()); err == nil && len(endpoints) > 0 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		http.Error(w, "no ready vMCP endpoints", http.StatusServiceUnavailable)
		return
	}

	endpoints, err := r.source.Endpoints(req.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	clientSession := req.Header.Get(r.sessionHeader)
	upstreamSession := ""
	var target endpoint
	if clientSession == "" {
		target = r.pick(endpoints)
	} else {
		var address string
		address, upstreamSession, err = decodeSession(clientSession)
		if err != nil || !containsEndpoint(endpoints, address) {
			// MCP Streamable HTTP specifies 404 for an unknown or expired session.
			http.Error(w, "MCP session is no longer available; initialize a new session", http.StatusNotFound)
			return
		}
		for _, candidate := range endpoints {
			if candidate.Address == address {
				target = candidate
				break
			}
		}
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(proxyReq *httputil.ProxyRequest) {
			proxyReq.SetURL(&url.URL{Scheme: "http", Host: target.Address + ":" + strconv.Itoa(r.backendPort)})
			proxyReq.SetXForwarded()
			if upstreamSession == "" {
				proxyReq.Out.Header.Del(r.sessionHeader)
			} else {
				proxyReq.Out.Header.Set(r.sessionHeader, upstreamSession)
			}
		},
		Transport:     r.transport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if session := resp.Header.Get(r.sessionHeader); session != "" {
				resp.Header.Set(r.sessionHeader, encodeSession(target.Address, session))
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, proxyErr error) {
			log.Printf("upstream %s failed: %v", target.Address, proxyErr)
			http.Error(w, "vMCP upstream unavailable", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, req)
}

func (r *router) pick(endpoints []endpoint) endpoint {
	local := make([]endpoint, 0, len(endpoints))
	for _, candidate := range endpoints {
		if r.nodeName != "" && candidate.Node == r.nodeName {
			local = append(local, candidate)
		}
	}
	pool := endpoints
	if len(local) > 0 {
		pool = local
	}
	return pool[(r.next.Add(1)-1)%uint64(len(pool))]
}

func encodeSession(address, session string) string {
	encode := base64.RawURLEncoding.EncodeToString
	return tokenPrefix + encode([]byte(address)) + "." + encode([]byte(session))
}

func decodeSession(value string) (string, string, error) {
	if !strings.HasPrefix(value, tokenPrefix) {
		return "", "", errors.New("session was not issued by ConnecTool")
	}
	parts := strings.Split(strings.TrimPrefix(value, tokenPrefix), ".")
	if len(parts) != 2 {
		return "", "", errors.New("malformed session")
	}
	address, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", err
	}
	session, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(address) == 0 || len(session) == 0 {
		return "", "", errors.New("malformed session")
	}
	return string(address), string(session), nil
}

func containsEndpoint(endpoints []endpoint, address string) bool {
	for _, candidate := range endpoints {
		if candidate.Address == address {
			return true
		}
	}
	return false
}

func main() {
	namespace := env("NAMESPACE", readFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"))
	service := os.Getenv("BACKEND_SERVICE")
	if namespace == "" || service == "" {
		log.Fatal("NAMESPACE and BACKEND_SERVICE are required")
	}
	port, err := strconv.Atoi(env("BACKEND_PORT", "4483"))
	if err != nil || port < 1 || port > 65535 {
		log.Fatal("BACKEND_PORT must be a valid TCP port")
	}
	token := readFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	caPEM, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		log.Fatalf("read Kubernetes CA: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		log.Fatal("parse Kubernetes CA")
	}
	apiURL := "https://kubernetes.default.svc/apis/discovery.k8s.io/v1/namespaces/" +
		url.PathEscape(namespace) + "/endpointslices?labelSelector=" +
		url.QueryEscape("kubernetes.io/service-name="+service)
	source := &kubeEndpointSource{
		client:   &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}},
		url:      apiURL,
		token:    token,
		cacheTTL: 2 * time.Second,
	}
	handler := &router{
		source: source, nodeName: os.Getenv("POD_NODE_NAME"), backendPort: port,
		sessionHeader: env("SESSION_HEADER", "Mcp-Session-Id"),
		transport:     &http.Transport{MaxIdleConns: 100, MaxIdleConnsPerHost: 20, IdleConnTimeout: 90 * time.Second},
	}
	server := &http.Server{Addr: env("LISTEN_ADDR", ":8080"), Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("routing %s/%s on %s", namespace, service, server.Addr)
	log.Fatal(server.ListenAndServe())
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
