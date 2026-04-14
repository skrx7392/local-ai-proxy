package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/krishna/local-ai-proxy/internal/admin"
	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/bootstrap"
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

// poolStatProvider adapts *pgxpool.Pool to appmetrics.PoolStatProvider,
// keeping the metrics package free of a pgx dependency.
type poolStatProvider struct{ pool *pgxpool.Pool }

func (p poolStatProvider) Stat() appmetrics.PoolStat {
	s := p.pool.Stat()
	return appmetrics.PoolStat{
		Total:            s.TotalConns(),
		Acquired:         s.AcquiredConns(),
		Idle:             s.IdleConns(),
		Max:              s.MaxConns(),
		Constructing:     s.ConstructingConns(),
		AcquireCount:     s.AcquireCount(),
		AcquireDuration:  s.AcquireDuration(),
		NewConns:         s.NewConnsCount(),
		LifetimeDestroys: s.MaxLifetimeDestroyCount(),
		IdleDestroys:     s.MaxIdleDestroyCount(),
		EmptyAcquires:    s.EmptyAcquireCount(),
	}
}

// Populated at build time via -ldflags "-X main.version=... -X main.buildTime=...".
// See deploy/Dockerfile. Must be string-typed package-level vars for -X to
// take effect; otherwise the flag is silently ignored and these stay at
// their defaults.
var (
	version   = "dev"
	buildTime = "unknown"
)

// usageChannelCapacity is the buffered size of the async usage-log channel.
// Reported by /api/admin/config and used as the saturation threshold by both
// the readiness probe and /api/admin/health.
const usageChannelCapacity = 1000

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

	startTime := time.Now()

	// Async usage logging channel
	usageCh := make(chan store.UsageEntry, usageChannelCapacity)

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

	// Backfill registration_events for pre-existing users and service accounts
	if err := db.BackfillRegistrationEvents(); err != nil {
		slog.Error("backfill registration events error", "error", err)
		os.Exit(1)
	}

	// Seed default model pricing (idempotent)
	if err := credits.SeedDefaultPricing(db); err != nil {
		slog.Error("seed pricing error", "error", err)
		os.Exit(1)
	}

	m := appmetrics.New(func() int { return len(usageCh) })
	m.RegisterPoolCollector(poolStatProvider{pool: db.Pool()})

	// Start credit hold sweeper goroutines (after metrics so counters are wired)
	sweeperCtx, sweeperCancel := context.WithCancel(context.Background())
	credits.StartSweeper(sweeperCtx, db, m,
		10*time.Minute, 10*time.Minute, // stale hold sweep every 10 min
		6*time.Hour, 30*24*time.Hour, // cleanup old holds every 6 hrs
	)

	limiter := ratelimit.New()
	proxyHandler := proxy.NewHandler(ollamaURL, usageCh, cfg.MaxRequestBody, db, m)
	authMiddleware := auth.Middleware(db)
	creditGate := credits.CreditGate(db, m)
	rateLimitMiddleware := ratelimit.Middleware(limiter, m)
	cors := middleware.CORS(cfg.CORSOrigins)

	hc := health.NewChecker(db, cfg.OllamaURL, func() int { return len(usageCh) }, cap(usageCh))
	hc.SetOllamaGauge(m.OllamaUp)

	configSnapshot := admin.ConfigSnapshot{
		OllamaURL:               cfg.OllamaURL,
		Port:                    cfg.Port,
		LogLevel:                cfg.LogLevel,
		MaxRequestBodyBytes:     cfg.MaxRequestBody,
		DefaultCreditGrant:      cfg.DefaultCreditGrant,
		CORSOrigins:             cfg.CORSOrigins,
		AdminRateLimitPerMinute: admin.AdminKeyRateLimitPerMinute,
		UsageChannelCapacity:    usageChannelCapacity,
		AdminSessionDurationHrs: int(user.AdminSessionDuration / time.Hour),
		UserSessionDurationHrs:  int(user.UserSessionDuration / time.Hour),
		Version:                 version,
		BuildTime:               buildTime,
		GoVersion:               runtime.Version(),
	}

	adminHandler := admin.NewHandler(db, cfg.AdminKey, usageCh, admin.Options{
		Snapshot:  configSnapshot,
		Checker:   hc,
		StartTime: startTime,
		Metrics:   m,
	})
	bootstrapHandler := bootstrap.New(db, cfg.AdminBootstrapToken, m)
	userHandler := user.NewHandler(db, cfg.DefaultCreditGrant, m)

	mux := http.NewServeMux()

	// Health checks — no auth, no CORS
	mux.HandleFunc("GET /api/healthz/live", hc.LiveHandler)
	mux.HandleFunc("GET /api/healthz/ready", hc.ReadyHandler)
	mux.HandleFunc("GET /api/healthz", hc.LiveHandler) // backward compat alias

	// Client API — CORS + auth + credit gate + rate limit + proxy (instrumented)
	mux.Handle("/api/v1/", m.InstrumentHandler(cors(authMiddleware(creditGate(rateLimitMiddleware(proxyHandler))))))

	// Metrics endpoint — unauthenticated, cluster-internal only
	mux.Handle("GET /metrics", m.Handler())

	// Admin bootstrap — mounted before the admin prefix so Go's ServeMux
	// routes POST /api/admin/bootstrap outside the admin authMiddleware.
	// Returns 404 unless ADMIN_BOOTSTRAP_TOKEN is set.
	mux.Handle("POST /api/admin/bootstrap", bootstrapHandler)

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
		slog.Info("proxy listening", "port", cfg.Port, "version", version, "build_time", buildTime)
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
