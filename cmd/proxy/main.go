package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/krishna/local-ai-proxy/internal/admin"
	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/config"
	"github.com/krishna/local-ai-proxy/internal/credits"
	"github.com/krishna/local-ai-proxy/internal/health"
	"github.com/krishna/local-ai-proxy/internal/logging"
	appmetrics "github.com/krishna/local-ai-proxy/internal/metrics"
	"github.com/krishna/local-ai-proxy/internal/middleware"
	"github.com/krishna/local-ai-proxy/internal/proxy"
	"github.com/krishna/local-ai-proxy/internal/ratelimit"
	"github.com/krishna/local-ai-proxy/internal/requestid"
	"github.com/krishna/local-ai-proxy/internal/store"
	"github.com/krishna/local-ai-proxy/internal/user"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config error", "error", err)
		os.Exit(1)
	}

	logger, err := logging.Setup(cfg.LogLevel)
	if err != nil {
		slog.Error("invalid log level", "error", err)
		os.Exit(1)
	}
	slog.SetDefault(logger)

	ollamaURL, err := url.Parse(cfg.OllamaURL)
	if err != nil {
		slog.Error("invalid OLLAMA_URL", "error", err)
		os.Exit(1)
	}

	db, err := store.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		slog.Error("store error", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// Async usage logging channel
	usageCh := make(chan store.UsageEntry, 1000)

	// Start async usage writer
	writerCtx, writerCancel := context.WithCancel(context.Background())
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case entry, ok := <-usageCh:
				if !ok {
					return
				}
				if err := db.LogUsage(entry); err != nil {
					slog.Error("usage log error", "error", err)
				}
			case <-writerCtx.Done():
				// Drain remaining entries
				for {
					select {
					case entry, ok := <-usageCh:
						if !ok {
							return
						}
						if err := db.LogUsage(entry); err != nil {
							slog.Error("usage log error (drain)", "error", err)
						}
					default:
						return
					}
				}
			}
		}
	}()

	// Backfill accounts for existing users (idempotent)
	if err := db.BackfillAccounts(); err != nil {
		slog.Error("backfill accounts error", "error", err)
		os.Exit(1)
	}

	// Backfill credit balances for accounts that lack one
	if err := db.BackfillCreditBalances(); err != nil {
		slog.Error("backfill credit balances error", "error", err)
		os.Exit(1)
	}

	// Seed default model pricing (idempotent)
	if err := credits.SeedDefaultPricing(db); err != nil {
		slog.Error("seed pricing error", "error", err)
		os.Exit(1)
	}

	// Start credit hold sweeper goroutines
	sweeperCtx, sweeperCancel := context.WithCancel(context.Background())
	credits.StartSweeper(sweeperCtx, db,
		10*time.Minute, 10*time.Minute, // stale hold sweep every 10 min
		6*time.Hour, 30*24*time.Hour, // cleanup old holds every 6 hrs
	)

	m := appmetrics.New(func() int { return len(usageCh) })

	limiter := ratelimit.New()
	proxyHandler := proxy.NewHandler(ollamaURL, usageCh, cfg.MaxRequestBody, db, m)
	authMiddleware := auth.Middleware(db)
	creditGate := credits.CreditGate(db, m)
	rateLimitMiddleware := ratelimit.Middleware(limiter, m)
	cors := middleware.CORS(cfg.CORSOrigins)
	adminHandler := admin.NewHandler(db, cfg.AdminKey, usageCh)
	userHandler := user.NewHandler(db, cfg.DefaultCreditGrant)

	hc := health.NewChecker(db, cfg.OllamaURL, func() int { return len(usageCh) }, cap(usageCh))
	hc.SetOllamaGauge(m.OllamaUp)

	mux := http.NewServeMux()

	// Health checks — no auth, no CORS
	mux.HandleFunc("GET /api/healthz/live", hc.LiveHandler)
	mux.HandleFunc("GET /api/healthz/ready", hc.ReadyHandler)
	mux.HandleFunc("GET /api/healthz", hc.LiveHandler) // backward compat alias

	// Client API — CORS + auth + credit gate + rate limit + proxy (instrumented)
	mux.Handle("/api/v1/", m.InstrumentHandler(cors(authMiddleware(creditGate(rateLimitMiddleware(proxyHandler))))))

	// Metrics endpoint — unauthenticated, cluster-internal only
	mux.Handle("GET /metrics", m.Handler())

	// Admin — no CORS
	mux.Handle("/api/admin/", adminHandler)

	// User API — CORS, session auth handled internally
	mux.Handle("/api/auth/", cors(userHandler))
	mux.Handle("/api/users/", cors(userHandler))

	// Service account registration — CORS, public (token-gated internally)
	mux.Handle("/api/accounts/", cors(userHandler))

	srv := &http.Server{
		Addr:        ":" + cfg.Port,
		Handler:     requestid.Middleware(mux),
		ReadTimeout: 30 * time.Second,
		IdleTimeout: 120 * time.Second,
		// WriteTimeout = 0: SSE streams can run indefinitely
	}

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("proxy listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)

	// Stop sweeper goroutines
	sweeperCancel()

	// Stop usage writer and drain
	writerCancel()
	close(usageCh)
	<-writerDone

	slog.Info("shutdown complete")
}
