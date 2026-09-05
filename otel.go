package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// OTel HTTP-server instruments (semantic-convention names). Nil until initOTel
// succeeds, so the middleware is a no-op when telemetry is disabled.
var (
	reqDuration    metric.Float64Histogram
	activeReqs     metric.Int64UpDownCounter
	entriesCreated metric.Int64Counter
	usersCreated   metric.Int64Counter
)

// initOTel wires up OTLP metric export (push) to the collector. Endpoint,
// protocol, and TLS come from the standard OTEL_EXPORTER_OTLP_* env vars; when
// none is set it's a no-op so local runs don't need a collector.
func initOTel(ctx context.Context) (shutdown func(context.Context) error, enabled bool) {
	noop := func(context.Context) error { return nil }
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") == "" {
		return noop, false
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(env("OTEL_SERVICE_NAME", "cashflow"))),
	)
	if err != nil {
		log.Printf("otel resource: %v", err)
	}

	exp, err := otlpmetrichttp.New(ctx) // endpoint/insecure read from OTEL_EXPORTER_OTLP_* env
	if err != nil {
		log.Printf("otel exporter: %v (metrics disabled)", err)
		return noop, false
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(15*time.Second))),
	)
	otel.SetMeterProvider(mp)

	meter := mp.Meter("cashflow")
	reqDuration, err = meter.Float64Histogram(
		"http.server.request.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of inbound HTTP requests."),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10),
	)
	if err != nil {
		log.Printf("otel histogram: %v", err)
	}
	activeReqs, err = meter.Int64UpDownCounter(
		"http.server.active_requests",
		metric.WithDescription("In-flight HTTP requests."),
	)
	if err != nil {
		log.Printf("otel counter: %v", err)
	}
	entriesCreated, err = meter.Int64Counter(
		"cashflow.entries.created",
		metric.WithDescription("Entries created (income/expense records)."),
	)
	if err != nil {
		log.Printf("otel entries counter: %v", err)
	}
	usersCreated, err = meter.Int64Counter(
		"cashflow.users.created",
		metric.WithDescription("User registrations."),
	)
	if err != nil {
		log.Printf("otel users counter: %v", err)
	}

	// Go runtime metrics (memory, goroutines, GC).
	if err := runtime.Start(runtime.WithMinimumReadMemStatsInterval(time.Second)); err != nil {
		log.Printf("otel runtime: %v", err)
	}

	return mp.Shutdown, true
}

// otelMiddleware records the HTTP server duration histogram per matched route.
// It must wrap the mux directly (inside withUser) so r.Pattern is populated.
func otelMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqDuration == nil { // telemetry disabled
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		activeReqs.Add(ctx, 1)
		defer activeReqs.Add(ctx, -1)

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)

		route := r.Pattern // e.g. "GET /p/{slug}"; empty when nothing matched
		if route == "" {
			route = "unmatched"
		}
		reqDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(
			semconv.HTTPRequestMethodKey.String(r.Method),
			semconv.HTTPRoute(route),
			semconv.HTTPResponseStatusCodeKey.Int(sw.status),
		))
	})
}

// recordEntryCreated increments the entry-creation counter, tagged by type
// ("income"/"expense"). Rate this metric to spot bursts/flooding. No-op when
// telemetry is disabled.
func recordEntryCreated(ctx context.Context, typ string) {
	if entriesCreated == nil {
		return
	}
	entriesCreated.Add(ctx, 1, metric.WithAttributes(attribute.String("type", typ)))
}

// recordUserCreated increments the registration counter (use increase()[7d] for
// new users this week). No-op when telemetry is disabled.
func recordUserCreated(ctx context.Context) {
	if usersCreated == nil {
		return
	}
	usersCreated.Add(ctx, 1)
}

// registerBusinessMetrics adds gauges for total users, cashplans and entries,
// sampled from the DB on each metric collection. Safe to call when OTel is
// disabled (the global meter is then a no-op and the callbacks never run).
func registerBusinessMetrics(store *Store) {
	meter := otel.Meter("cashflow")
	if _, err := meter.Int64ObservableGauge("cashflow.users",
		metric.WithDescription("Registered users."),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			n, err := store.CountUsers(ctx)
			if err != nil {
				return err
			}
			o.Observe(n)
			return nil
		}),
	); err != nil {
		log.Printf("otel users gauge: %v", err)
	}
	if _, err := meter.Int64ObservableGauge("cashflow.cashplans",
		metric.WithDescription("Cash plans created."),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			n, err := store.CountCashplans(ctx)
			if err != nil {
				return err
			}
			o.Observe(n)
			return nil
		}),
	); err != nil {
		log.Printf("otel cashplans gauge: %v", err)
	}
	if _, err := meter.Int64ObservableGauge("cashflow.entries",
		metric.WithDescription("Total entries recorded."),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			n, err := store.CountEntries(ctx)
			if err != nil {
				return err
			}
			o.Observe(n)
			return nil
		}),
	); err != nil {
		log.Printf("otel entries gauge: %v", err)
	}
}
