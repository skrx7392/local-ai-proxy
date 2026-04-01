package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/krishna/local-ai-proxy/internal/admin"
	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/config"
	"github.com/krishna/local-ai-proxy/internal/middleware"
	"github.com/krishna/local-ai-proxy/internal/proxy"
	"github.com/krishna/local-ai-proxy/internal/ratelimit"
	"github.com/krishna/local-ai-proxy/internal/store"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	db, err := store.New(cfg.DBPath)
	if err != nil {
		log.Fatalf("store: %v", err)
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
					log.Printf("usage log error: %v", err)
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
							log.Printf("usage log error (drain): %v", err)
						}
					default:
						return
					}
				}
			}
		}
	}()

	limiter := ratelimit.New()
	proxyHandler := proxy.NewHandler(cfg.OllamaURL, usageCh, cfg.MaxRequestBody)
	authMiddleware := auth.Middleware(db)
	rateLimitMiddleware := ratelimit.Middleware(limiter)
	cors := middleware.CORS(cfg.CORSOrigins)
	adminHandler := admin.NewHandler(db, cfg.AdminKey, usageCh)

	mux := http.NewServeMux()

	// Health check — no auth, no CORS
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Client API — CORS + auth + rate limit + proxy
	mux.Handle("/v1/", cors(authMiddleware(rateLimitMiddleware(proxyHandler))))

	// Admin — no CORS
	mux.Handle("/admin/", adminHandler)

	srv := &http.Server{
		Addr:        ":" + cfg.Port,
		Handler:     mux,
		ReadTimeout: 30 * time.Second,
		IdleTimeout: 120 * time.Second,
		// WriteTimeout = 0: SSE streams can run indefinitely
	}

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("proxy listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)

	// Stop usage writer and drain
	writerCancel()
	close(usageCh)
	<-writerDone

	log.Println("shutdown complete")
}
