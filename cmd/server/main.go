package main

import (
    "context"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/go-chi/chi/v5"
    chiMiddleware "github.com/go-chi/chi/v5/middleware"
    "github.com/go-chi/httprate"
    "github.com/prometheus/client_golang/prometheus/promhttp"
    "github.com/rs/zerolog"
    "github.com/rs/zerolog/log"

    "github.com/Natthyx/payment-gateway/internal/audit"
    "github.com/Natthyx/payment-gateway/internal/config"
    appdb "github.com/Natthyx/payment-gateway/internal/db"
    "github.com/Natthyx/payment-gateway/internal/ledger"
    "github.com/Natthyx/payment-gateway/internal/merchant"
    "github.com/Natthyx/payment-gateway/internal/metrics"
    "github.com/Natthyx/payment-gateway/internal/middleware"
    "github.com/Natthyx/payment-gateway/internal/outbound"
    "github.com/Natthyx/payment-gateway/internal/payment"
    "github.com/Natthyx/payment-gateway/internal/provider"
    provChapa "github.com/Natthyx/payment-gateway/internal/provider/chapa"
    provMock "github.com/Natthyx/payment-gateway/internal/provider/mock"
    provStripe "github.com/Natthyx/payment-gateway/internal/provider/stripe"
    provTelebirr "github.com/Natthyx/payment-gateway/internal/provider/telebirr"
    "github.com/Natthyx/payment-gateway/internal/reconcile"
    "github.com/Natthyx/payment-gateway/internal/webhook"
)

func main() {
    if os.Getenv("PRETTY") == "true" {
        log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
    } else {
        zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
    }
    log.Info().Msg("starting payment gateway")

    cfg, err := config.Load()
    if err != nil {
        log.Fatal().Err(err).Msg("failed to load config")
    }
    level, err := zerolog.ParseLevel(cfg.LogLevel)
    if err != nil {
        log.Fatal().Err(err).Str("LOG_LEVEL", cfg.LogLevel).Msg("invalid LOG_LEVEL")
    }
    zerolog.SetGlobalLevel(level)

    // Migrations run as the table owner; the app itself can use a
    // least-privilege role (see migrations/010_append_only_hardening.up.sql).
    if err := appdb.RunMigrations(cfg.MigrationsPath, cfg.MigrationsDatabaseURL); err != nil {
        log.Fatal().Err(err).Msg("failed to apply migrations")
    }

    ctx := context.Background()
    pgPool, err := appdb.NewPool(ctx, cfg.DatabaseURL)
    if err != nil {
        log.Fatal().Err(err).Msg("failed to connect to PostgreSQL")
    }
    defer pgPool.Close()

    redisClient, err := appdb.NewRedis(ctx, cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
    if err != nil {
        log.Fatal().Err(err).Msg("failed to connect to Redis")
    }
    defer redisClient.Close()

    idempotencyStore := appdb.NewIdempotencyStore(redisClient)

    registry := provider.NewRegistry()
    stripeClient := provStripe.New(cfg.StripeSecretKey, cfg.StripeWebhookSecret)
    chapaClient := provChapa.New(cfg.ChapaSecretKey)
    telebirrClient := provTelebirr.New(
        cfg.TelebirrAppID, cfg.TelebirrAppKey, cfg.TelebirrPublicKey, cfg.TelebirrShortCode,
    )
    if cfg.ProviderMode == "mock" {
        // Load-test mode: no provider network calls. Webhook signatures are
        // still verified by the real clients.
        log.Warn().Dur("latency", cfg.MockLatency).Str("tx_status", cfg.MockTxStatus).
            Msg("PROVIDER_MODE=mock — using in-process mock providers")
        registry.Register(provMock.New(stripeClient.Name(), stripeClient, cfg.MockLatency, cfg.MockTxStatus))
        registry.Register(provMock.New(chapaClient.Name(), chapaClient, cfg.MockLatency, cfg.MockTxStatus))
        registry.Register(provMock.New(telebirrClient.Name(), telebirrClient, cfg.MockLatency, cfg.MockTxStatus))
    } else {
        registry.Register(stripeClient)
        registry.Register(chapaClient)
        registry.Register(telebirrClient)
    }

    auditSvc := audit.NewService(pgPool)
    merchantSvc := merchant.NewService(pgPool, cfg.Env)
    merchantSvc.SetAuthCacheTTL(cfg.AuthCacheTTL)
    ledgerSvc := ledger.NewService(pgPool)
    outboundSvc := outbound.NewService(pgPool, cfg.OutboundWebhookSigningSecret)
    paymentSvc := payment.NewService(pgPool, registry, ledgerSvc, idempotencyStore, cfg.FeePercent, cfg.CallbackURL)
    refundSvc := payment.NewRefundService(pgPool, registry, ledgerSvc, auditSvc, outboundSvc)
    webhookSvc := webhook.NewService(pgPool, registry, ledgerSvc, outboundSvc, cfg.FeePercent)
    reconcileSvc := reconcile.NewService(pgPool, registry, ledgerSvc, outboundSvc, cfg.FeePercent)

    merchantHandler := merchant.NewHandler(merchantSvc, auditSvc)
    paymentHandler := payment.NewHandler(paymentSvc, refundSvc, ledgerSvc)
    webhookHandler := webhook.NewHandler(webhookSvc)
    outboundHandler := outbound.NewHandler(outboundSvc)

    r := chi.NewRouter()
    r.Use(middleware.Recoverer)
    r.Use(middleware.Logger)
    // Only trust client-IP headers when a proxy we control sets them;
    // otherwise any client could choose its own IP for the per-IP limit.
    if cfg.TrustProxyHeaders {
        r.Use(chiMiddleware.RealIP)
    }
    r.Use(chiMiddleware.RequestID)
    r.Use(httprate.Limit(cfg.IPRateLimitPerMinute, time.Minute,
        httprate.WithKeyFuncs(httprate.KeyByIP),
        httprate.WithLimitHandler(middleware.IPRateLimitExceeded),
    ))
    r.Use(chiMiddleware.Timeout(30 * time.Second))
    r.Use(metrics.Instrument)

    perMerchantLimit := middleware.NewPerMerchantRateLimiter(redisClient, cfg.RateLimitPerMerchantPerMinute)

    r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        _, _ = w.Write([]byte(`{"status":"ok","service":"payment-gateway"}`))
    })
    r.Method(http.MethodGet, "/metrics", promhttp.Handler())

    r.Route("/merchants", merchantHandler.Routes())
    r.Route("/webhooks", webhookHandler.Routes())

    r.Group(func(r chi.Router) {
        r.Use(middleware.AuthenticateMerchant(merchantSvc))
        r.Use(perMerchantLimit.Handler)
        r.Route("/payments", paymentHandler.Routes())
        r.Route("/webhook-endpoints", outboundHandler.Routes())
    })

    workerCtx, cancelWorkers := context.WithCancel(ctx)
    defer cancelWorkers()
    go webhookSvc.StartWorker(workerCtx, 10*time.Second)
    go outboundSvc.Start(workerCtx, 5*time.Second)
    go reconcileSvc.Start(workerCtx, 5*time.Minute, 15*time.Minute)

    srv := &http.Server{
        Addr:         ":" + cfg.Port,
        Handler:      r,
        ReadTimeout:  cfg.ReadTimeout,
        WriteTimeout: cfg.WriteTimeout,
        IdleTimeout:  120 * time.Second,
    }

    go func() {
        log.Info().Str("port", cfg.Port).Msg("HTTP server listening")
        if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            log.Fatal().Err(err).Msg("server error")
        }
    }()

    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
    <-quit
    log.Info().Msg("shutdown signal received")
    cancelWorkers()

    shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    if err := srv.Shutdown(shutdownCtx); err != nil {
        log.Error().Err(err).Msg("forced shutdown")
    }
    log.Info().Msg("server stopped cleanly")
}
