package main

import (
	"bufio"
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "time/tzdata" // embed the tz database so Asia/Jakarta always resolves

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed templates
var tmplFS embed.FS

//go:embed static
var staticEmbed embed.FS

//go:embed schema.sql
var schemaSQL string

var jakarta *time.Location

func init() {
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		loc = time.FixedZone("WIB", 7*3600)
	}
	jakarta = loc
}

func main() {
	loadDotenv(".env") // local-dev convenience; Docker uses compose env_file

	dsn := env("DATABASE_URL", "postgres://cashflow:cashflow@localhost:5432/cashflow?sslmode=disable")
	port := env("PORT", "8080")

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}
	defer pool.Close()

	if err := waitForDB(ctx, pool); err != nil {
		log.Fatalf("database not ready: %v", err)
	}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		log.Fatalf("apply schema: %v", err)
	}
	log.Print("database ready, schema applied")

	nc := NewNextcloudFromEnv()
	if nc.Enabled() {
		log.Print("nextcloud uploads: enabled")
	} else {
		log.Print("nextcloud uploads: disabled (set NEXTCLOUD_URL/USER/APP_PASSWORD to enable)")
	}

	otelShutdown, otelOn := initOTel(ctx)
	if otelOn {
		log.Print("otel metrics: enabled (OTLP export)")
	} else {
		log.Print("otel metrics: disabled (set OTEL_EXPORTER_OTLP_ENDPOINT to enable)")
	}

	app := &App{store: &Store{pool: pool}, tmpl: buildTemplates(), nc: nc}

	staticFS, err := fs.Sub(staticEmbed, "static")
	if err != nil {
		log.Fatalf("static fs: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", app.handleHome)
	// Auth
	mux.HandleFunc("GET /register", app.handleRegisterForm)
	mux.HandleFunc("POST /register", app.handleRegister)
	mux.HandleFunc("GET /login", app.handleLoginForm)
	mux.HandleFunc("POST /login", app.handleLogin)
	mux.HandleFunc("POST /logout", app.handleLogout)
	// Cashplans (owner)
	mux.HandleFunc("POST /cashplans", app.handleCreate)
	mux.HandleFunc("GET /kelola/{slug}", app.handleManage)
	mux.HandleFunc("POST /kelola/{slug}/entry", app.handleAddEntry)
	mux.HandleFunc("GET /kelola/{slug}/laporan", app.handleManageReport)
	// Public view
	mux.HandleFunc("GET /p/{slug}", app.handleView)
	mux.HandleFunc("GET /p/{slug}/laporan", app.handleViewReport)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)))

	go pruneSessions(pool) // periodically delete expired sessions

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           logRequests(securityHeaders(app.withUser(otelMiddleware(mux)))),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,  // generous: allows a slow ~5 MB receipt upload
		WriteTimeout:      120 * time.Second, // generous: allows the Nextcloud round-trip
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()
	log.Printf("cashflow listening on http://localhost:%s", port)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Print("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	_ = otelShutdown(shutCtx)
}

// securityHeaders sets conservative, app-wide response headers. The CSP is
// strict (default-src 'self'); it works because all CSS/JS is same-origin and
// there are no inline scripts/handlers. The favicon is a data: URI (img-src).
func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; img-src 'self' data:; base-uri 'none'; " +
		"form-action 'self'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// pruneSessions deletes expired sessions at startup and hourly thereafter.
func pruneSessions(pool *pgxpool.Pool) {
	for {
		if n, err := (&Store{pool: pool}).DeleteExpiredSessions(context.Background()); err != nil {
			log.Printf("session cleanup: %v", err)
		} else if n > 0 {
			log.Printf("session cleanup: removed %d expired", n)
		}
		time.Sleep(time.Hour)
	}
}

func waitForDB(ctx context.Context, pool *pgxpool.Pool) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := pool.Ping(ctx); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after 30s")
		}
		time.Sleep(time.Second)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadDotenv reads simple KEY=VALUE lines from a .env file (if present) and sets
// any variables not already in the environment. Minimal on purpose: no export
// keyword, no interpolation. Missing file is fine.
func loadDotenv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
	})
}
