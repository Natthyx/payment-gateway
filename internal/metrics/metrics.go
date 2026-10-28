// Package metrics centralises Prometheus instrumentation.
// One labelled HTTP histogram per (method, route, status_class) keeps cardinality bounded.
package metrics

import (
    "net/http"
    "strconv"
    "time"

    "github.com/go-chi/chi/v5"
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)

var (
    HTTPRequests = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "gateway_http_requests_total",
        Help: "Total HTTP requests handled by the gateway.",
    }, []string{"method", "route", "status_class"})

    HTTPLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Name:    "gateway_http_request_duration_seconds",
        Help:    "End-to-end HTTP request latency.",
        Buckets: prometheus.DefBuckets,
    }, []string{"method", "route", "status_class"})

    PaymentsCreated = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "gateway_payments_created_total",
        Help: "Payment intents created, partitioned by provider+currency.",
    }, []string{"provider", "currency"})

    PaymentsByStatus = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "gateway_payments_status_transitions_total",
        Help: "Status transitions of payment intents.",
    }, []string{"provider", "from", "to"})

    WebhookProcessed = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "gateway_webhooks_processed_total",
        Help: "Webhook outcomes from the inbox worker.",
    }, []string{"provider", "outcome"}) // outcome=processed|failed|dropped

    OutboundDeliveries = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "gateway_outbound_webhook_attempts_total",
        Help: "Attempts to deliver webhooks to merchant endpoints.",
    }, []string{"outcome"}) // outcome=success|http_error|network_error

    LedgerEntries = promauto.NewCounter(prometheus.CounterOpts{
        Name: "gateway_ledger_entries_written_total",
        Help: "Ledger entries committed.",
    })
)

// Instrument wraps an HTTP handler with request/latency counters.
// Uses the chi route pattern (not raw path) to keep cardinality bounded.
func Instrument(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
        next.ServeHTTP(rw, r)
        route := chi.RouteContext(r.Context()).RoutePattern()
        if route == "" {
            route = "unmatched"
        }
        sc := strconv.Itoa(rw.status/100) + "xx"
        HTTPRequests.WithLabelValues(r.Method, route, sc).Inc()
        HTTPLatency.WithLabelValues(r.Method, route, sc).Observe(time.Since(start).Seconds())
    })
}

type statusRecorder struct {
    http.ResponseWriter
    status int
}

func (s *statusRecorder) WriteHeader(code int) {
    s.status = code
    s.ResponseWriter.WriteHeader(code)
}
