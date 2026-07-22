package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/krishna/local-ai-proxy/internal/admin"
	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/authlimit"
	"github.com/krishna/local-ai-proxy/internal/billing"
	"github.com/krishna/local-ai-proxy/internal/bootstrap"
	"github.com/krishna/local-ai-proxy/internal/config"
	"github.com/krishna/local-ai-proxy/internal/creditrequest"
	"github.com/krishna/local-ai-proxy/internal/credits"
	"github.com/krishna/local-ai-proxy/internal/health"
	"github.com/krishna/local-ai-proxy/internal/logging"
	appmetrics "github.com/krishna/local-ai-proxy/internal/metrics"
	"github.com/krishna/local-ai-proxy/internal/middleware"
	"github.com/krishna/local-ai-proxy/internal/nodesource"
	"github.com/krishna/local-ai-proxy/internal/poller"
	"github.com/krishna/local-ai-proxy/internal/proxy"
	"github.com/krishna/local-ai-proxy/internal/ratelimit"
	"github.com/krishna/local-ai-proxy/internal/registry"
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
		CanceledAcquires: s.CanceledAcquireCount(),
		EmptyAcquireWait: s.EmptyAcquireWaitTime(),
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

	db, err := store.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		slog.Error("store error", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// Reconcile declared nodes (NODES_FILE + OLLAMA_URL synthesis) into the
	// store before anything reads the node set. Fail fast: an invalid
	// declaration file must not boot a proxy with a partial node set.
	if err := nodesource.SyncDeclaredNodes(context.Background(), db, cfg); err != nil {
		slog.Error("node sync error", "error", err)
		os.Exit(1)
	}

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

	// Ensure the admin service account exists and attach any remaining
	// NULL-account API keys to it (idempotent). This removes the credit-gate
	// bypass: every key — including admin-minted ones — is account-backed
	// and metered. Live legacy keys keep working via the service account.
	adminAccountID, err := db.EnsureAdminServiceAccount(cfg.AdminServiceCreditGrant)
	if err != nil {
		slog.Error("ensure admin service account error", "error", err)
		os.Exit(1)
	}
	attached, err := db.BackfillAdminKeyAccounts(adminAccountID)
	if err != nil {
		slog.Error("backfill admin key accounts error", "error", err)
		os.Exit(1)
	}
	if attached > 0 {
		slog.Info("attached legacy API keys to accounts", "count", attached, "admin_account_id", adminAccountID)
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

	// Nothing seeds the pricing catalog: pricing your models is an explicit
	// setup step (POST /api/admin/pricing). Warn loudly when the catalog is
	// empty so operators know why /v1/models lists nothing.
	if err := credits.WarnIfPricingEmpty(db, slog.Default()); err != nil {
		slog.Error("pricing catalog check error", "error", err)
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

	// Node routing: registry + health poller. The synchronous startup sweep
	// probes all enabled nodes in parallel (bounded budget) BEFORE the HTTP
	// listener opens, so restarts route deterministically; the poller keeps
	// health and model lists current afterwards and maintains both
	// aiproxy_node_up{node} and the legacy aiproxy_ollama_up gauge (it is
	// the sole writer of the latter).
	reg := registry.New()
	nodePoller := poller.New(db, reg, m, poller.Options{})
	pollerCtx, pollerCancel := context.WithCancel(context.Background())
	defer pollerCancel()
	if err := nodePoller.SweepOnce(pollerCtx); err != nil {
		slog.Error("node startup sweep error", "error", err)
		os.Exit(1)
	}

	// Two limiter instances, deliberately separate: key IDs and account IDs
	// share the int64 space, so one map would collide key #7 with account #7
	// (docs/design/per-account-rate-limiting.md §3.1).
	keyLimiter := ratelimit.New()
	accountLimiter := ratelimit.New()
	capHits := creditrequest.New(db, cfg.CreditAlertWebhookURL, cfg.EndUserMonthlyGrant)
	proxyHandler := proxy.NewHandler(reg, usageCh, cfg.MaxRequestBody, db, m,
		proxy.Options{ModelsListAll: cfg.ModelsListAll, CapHits: capHits})
	authMiddleware := auth.Middleware(db)
	billingResolver := billing.Middleware(db, cfg.EndUserMonthlyGrant)
	creditGate := credits.CreditGate(db, m, capHits)
	rateLimitMiddleware := ratelimit.Middleware(keyLimiter, accountLimiter, ratelimit.Limits{
		EndUserPerMin: cfg.EndUserRateLimitPerMin,
		ServicePerMin: cfg.AccountRateLimitPerMin,
	}, m)
	cors := middleware.CORS(cfg.CORSOrigins)

	// Brute-force / bcrypt-DoS protection for the public auth surface.
	authGuard := authlimit.New(authlimit.Config{
		LoginPerMinIP:     cfg.AuthLoginPerMinIP,
		LoginPerMinEmail:  cfg.AuthLoginPerMinEmail,
		RegisterPerMinIP:  cfg.AuthRegisterPerMinIP,
		GeneralPerMinIP:   cfg.AuthGeneralPerMinIP,
		BcryptConcurrency: cfg.AuthBcryptConcurrency,
	})
	authLimit := authlimit.Middleware(authGuard, m)

	// 1MB body cap for the JSON API mounts; the /api/v1/ proxy keeps its
	// own 50MB cap (MAX_REQUEST_BODY).
	jsonBody := middleware.MaxBody(cfg.MaxJSONBody)

	// Readiness derives node health from the registry snapshot — no
	// synchronous probes. The aiproxy_ollama_up gauge is owned by the node
	// poller now; wiring the checker to it too would create two writers.
	hc := health.NewChecker(db, reg, func() int { return len(usageCh) }, cap(usageCh))

	configSnapshot := admin.ConfigSnapshot{
		// Raw OLLAMA_URL value; empty when unset (no synthesized node).
		OllamaURL:                        cfg.OllamaURL,
		Port:                             cfg.Port,
		LogLevel:                         cfg.LogLevel,
		MaxRequestBodyBytes:              cfg.MaxRequestBody,
		MaxJSONBodyBytes:                 cfg.MaxJSONBody,
		DefaultCreditGrant:               cfg.DefaultCreditGrant,
		CORSOrigins:                      cfg.CORSOrigins,
		AdminRateLimitPerMinute:          admin.AdminKeyRateLimitPerMinute,
		AuthLoginRateLimitPerMinute:      cfg.AuthLoginPerMinIP,
		AuthLoginEmailRateLimitPerMinute: cfg.AuthLoginPerMinEmail,
		AuthRegisterRateLimitPerMinute:   cfg.AuthRegisterPerMinIP,
		AuthGeneralRateLimitPerMinute:    cfg.AuthGeneralPerMinIP,
		AuthBcryptMaxConcurrent:          cfg.AuthBcryptConcurrency,
		AccountRateLimitPerMinute:        cfg.AccountRateLimitPerMin,
		EndUserRateLimitPerMinute:        cfg.EndUserRateLimitPerMin,
		UsageChannelCapacity:             usageChannelCapacity,
		AdminSessionDurationHrs:          int(user.AdminSessionDuration / time.Hour),
		UserSessionDurationHrs:           int(user.UserSessionDuration / time.Hour),
		Version:                          version,
		BuildTime:                        buildTime,
		GoVersion:                        runtime.Version(),
		ModelsListAll:                    cfg.ModelsListAll,
		NodesFile:                        cfg.NodesFile,
	}

	adminHandler := admin.NewHandler(db, cfg.AdminKey, usageCh, admin.Options{
		Snapshot:  configSnapshot,
		Checker:   hc,
		StartTime: startTime,
		Metrics:   m,
		Registry:  reg,
		Refresher: nodePoller,

		AdminServiceCreditGrant: cfg.AdminServiceCreditGrant,
		EndUserMonthlyGrant:     cfg.EndUserMonthlyGrant,
		AccountRateLimitPerMin:  cfg.AccountRateLimitPerMin,
		EndUserRateLimitPerMin:  cfg.EndUserRateLimitPerMin,
	})
	bootstrapHandler := bootstrap.New(db, cfg.AdminBootstrapToken, m)
	userHandler := user.NewHandler(db, cfg.DefaultCreditGrant, m, authGuard)

	mux := http.NewServeMux()

	// Health checks — no auth, no CORS
	mux.HandleFunc("GET /api/healthz/live", hc.LiveHandler)
	mux.HandleFunc("GET /api/healthz/ready", hc.ReadyHandler)
	mux.HandleFunc("GET /api/healthz", hc.LiveHandler) // backward compat alias

	// Client API — CORS + auth + billing resolution + rate limit + credit
	// gate + proxy (instrumented). Billing MUST precede both gates: they act
	// on the resolved billing account, not the key's own account
	// (docs/design/end-user-accounts.md §4). Rate limit precedes the credit
	// gate so 402-spam cannot drive unthrottled per-request credit-status
	// queries; consequence: an over-cap AND over-rate account sees 429, and
	// cap-hit recording (the credit-request trigger) fires on its first
	// rate-passing request (docs/design/per-account-rate-limiting.md §3.3).
	mux.Handle("/api/v1/", m.InstrumentHandler(cors(authMiddleware(billingResolver(rateLimitMiddleware(creditGate(proxyHandler)))))))

	// Metrics endpoint — unauthenticated, cluster-internal only
	mux.Handle("GET /metrics", m.Handler())

	// Admin bootstrap — mounted before the admin prefix so Go's ServeMux
	// routes POST /api/admin/bootstrap outside the admin authMiddleware.
	// Returns 404 unless ADMIN_BOOTSTRAP_TOKEN is set.
	mux.Handle("POST /api/admin/bootstrap", jsonBody(bootstrapHandler))

	// Admin — no CORS
	mux.Handle("/api/admin/", jsonBody(adminHandler))

	// User API — CORS, per-IP auth limits, body cap, session auth handled
	// internally. authLimit sits inside cors so OPTIONS preflights don't
	// consume tokens.
	mux.Handle("/api/auth/", cors(authLimit(jsonBody(userHandler))))
	mux.Handle("/api/users/", cors(authLimit(jsonBody(userHandler))))

	// Service account registration — CORS, public (token-gated internally)
	mux.Handle("/api/accounts/", cors(authLimit(jsonBody(userHandler))))

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

	// Start the ongoing node poll loop (the startup sweep already ran);
	// returns when pollerCtx is cancelled during shutdown.
	go nodePoller.Run(pollerCtx)

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

	// Stop node poller and sweeper goroutines
	pollerCancel()
	sweeperCancel()

	// Stop usage writer and drain
	writerCancel()
	close(usageCh)
	<-writerDone

	// Drain in-flight cap-hit recordings (filed rows + webhook deliveries)
	// before the deferred db.Close tears the pool down under them.
	capHits.Wait()

	slog.Info("shutdown complete")
}
