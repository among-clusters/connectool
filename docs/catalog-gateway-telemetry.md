# Catalog gateway telemetry

`GET /metrics` uses the existing configured HTTP listener. It is an internal
Prometheus text endpoint, not another Service or a public MCP route. Keep public
ingress matched to the explicit MCP path; do not publish `/metrics` through a
catch-all route. `/metrics` and `/livez` are excluded from request RED telemetry.

The official Prometheus Go client exports:

- `re8ch_toolchain_http_requests_total`
- `re8ch_toolchain_http_request_duration_seconds`
- `re8ch_toolchain_build_info`

HTTP series have exactly `component`, `environment`, `phase`, `method`,
`status_class`, `outcome`, and `error_category` labels. The component is
`connectool-gateway` and phase is `gateway_request`. HTTP methods and error
categories use bounded enums. A canceled or panicking request that never wrote
headers has status class `none`, rather than a fabricated HTTP success/status.
The duration histogram includes finite 120/300/600/1800/3600-second buckets.
It measures server-side completion, not proof that a client received the result.

Build info has only `component`, `release`, `repository`, and `environment`.
Identity comes from `SENTRY_RELEASE`, `SENTRY_REPOSITORY`, and
`SENTRY_ENVIRONMENT`; defaults are `unversioned`, the product repository, and
`production`. This allows a metrics-to-events link to join release identity
without placing high-cardinality identifiers on each HTTP series.

Each completed non-health/scrape request emits one structured JSON log with
`traceId`, `operationId`, `sentryEventId`, `gatewayRequestId`, `component`,
`environment`, `release`, phase, bounded outcome, status and duration. Empty event
IDs are retained when Sentry did not create an error event. No request/response
payload, arguments, caller identity, URL or header map is logged. Operation IDs
are validated before appearing in logs or Sentry tags. Trace/operation/event IDs
are never metrics labels.

The middleware preserves the existing response-controller unwrap path, streamed
flushes, caller cancellation and panic propagation. It does not flush Sentry on
the request path or replay business calls. Native Grafana data links can use
component/environment/release/time to open the existing Sentry UI; log fields
provide the more specific trace, operation and error-event pivots. Metrics
storage without exemplar support must not advertise exemplar-to-trace links.
