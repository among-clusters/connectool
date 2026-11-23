package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const gatewayComponent = "connectool-gateway"
const gatewayPhase = "gateway_request"

var operationIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,96}$`)

type gatewayMetrics struct {
	registry                         *prometheus.Registry
	handler                          http.Handler
	requests                         *prometheus.CounterVec
	duration                         *prometheus.HistogramVec
	environment, release, repository string
	logger                           *log.Logger
}

func (g *gateway) telemetry() *gatewayMetrics {
	g.telemetryOnce.Do(func() {
		m := &gatewayMetrics{registry: prometheus.NewRegistry(), environment: env("SENTRY_ENVIRONMENT", "production"), release: env("SENTRY_RELEASE", "unversioned"), repository: env("SENTRY_REPOSITORY", "among-clusters/connectool"), logger: log.New(os.Stdout, "", 0)}
		labels := []string{"component", "environment", "phase", "method", "status_class", "outcome", "error_category"}
		m.requests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "re8ch_toolchain_http_requests_total", Help: "Completed gateway HTTP requests, including failures before authentication."}, labels)
		m.duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "re8ch_toolchain_http_request_duration_seconds", Help: "Server-side gateway request duration; not proof the caller received the result.", Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800, 3600}}, labels)
		build := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "re8ch_toolchain_build_info", Help: "Deployed component release identity."}, []string{"component", "release", "repository", "environment"})
		build.WithLabelValues(gatewayComponent, m.release, m.repository, m.environment).Set(1)
		m.registry.MustRegister(m.requests, m.duration, build)
		m.handler = promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
		g.telemetryState = m
	})
	return g.telemetryState
}

func safeOperationID(value string) string {
	if operationIDPattern.MatchString(value) {
		return value
	}
	return ""
}

func boundedMethod(method string) string {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		return method
	default:
		return "OTHER"
	}
}

func boundedFailure(failure string) string {
	switch failure {
	case "", "catalog_read", "core_transport", "response_stream", "response_write", "rpc_failure", "http_failure", "caller_canceled", "panic":
		if failure == "" {
			return "none"
		}
		return failure
	default:
		return "other"
	}
}

func (m *gatewayMetrics) completed(r *http.Request, observed *diagnosticWriter, start time.Time, traceID, operationID, eventID, requestID string) {
	elapsed := time.Since(start)
	outcome := "success"
	if observed.failure != "" {
		outcome = "error"
	}
	if r.Context().Err() != nil {
		outcome = "canceled"
	}
	if observed.failure == "panic" {
		outcome = "panic"
	}
	status := observed.status
	if status == 0 && observed.failure == "" {
		status = http.StatusOK
	}
	statusClass := "none" // No fabricated 200/499 when a canceled or panicking request wrote no headers.
	if status >= 100 && status <= 599 {
		statusClass = strconv.Itoa(status/100) + "xx"
	}
	category := boundedFailure(observed.failure)
	labels := []string{gatewayComponent, m.environment, gatewayPhase, boundedMethod(r.Method), statusClass, outcome, category}
	m.requests.WithLabelValues(labels...).Inc()
	m.duration.WithLabelValues(labels...).Observe(elapsed.Seconds())
	line, _ := json.Marshal(map[string]any{"message": "MCP gateway request completed", "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "component": gatewayComponent, "environment": m.environment, "release": m.release, "phase": gatewayPhase, "method": boundedMethod(r.Method), "statusClass": statusClass, "httpStatus": status, "outcome": outcome, "errorCategory": category, "durationMs": elapsed.Milliseconds(), "traceId": traceID, "operationId": operationID, "sentryEventId": eventID, "gatewayRequestId": requestID})
	m.logger.Print(string(line))
}
