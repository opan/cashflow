package main

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prometheus metrics. The route label uses the matched ServeMux pattern
// (e.g. "GET /p/{slug}") — never the raw path — so slugs don't blow up label
// cardinality. Go runtime (go_*) and process_* metrics come for free from the
// default registry.
var (
	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cashflow_http_requests_total",
		Help: "Total HTTP requests, by method, route, and status code.",
	}, []string{"method", "route", "status"})

	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cashflow_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds, by method and route.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})

	httpInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cashflow_http_requests_in_flight",
		Help: "HTTP requests currently being served.",
	})
)

// metricsMiddleware records per-route request metrics. It must wrap the mux
// directly (inside withUser) so r.Pattern — set by ServeMux during routing — is
// populated on the request it holds by the time ServeHTTP returns.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpInFlight.Inc()
		defer httpInFlight.Dec()

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)

		route := r.Pattern // e.g. "GET /p/{slug}"; empty when nothing matched
		if route == "" {
			route = "unmatched"
		}
		httpRequests.WithLabelValues(r.Method, route, strconv.Itoa(sw.status)).Inc()
		httpDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
	})
}

// serveMetrics runs a separate listener that exposes /metrics. Keeping it off
// the main port means metrics are never reachable through the public ingress.
func serveMetrics(port string) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("metrics listening on :%s/metrics", port)
	if err := srv.ListenAndServe(); err != nil {
		log.Printf("metrics server: %v", err)
	}
}
