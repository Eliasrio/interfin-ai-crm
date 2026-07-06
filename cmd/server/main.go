// cmd/server — точка входа HTTP-сервера Interfin AI-CRM.
//
// M0: конфиг, Postgres (pgx SimpleProtocol, AQ²-fix #3), Redis,
// /health и /ready, graceful shutdown.
// M2: Telegram ingestion — telebot в webhook-режиме (CLAUDE.md §4.10),
// цепочка POST /webhook/telegram, Asynq-клиент (воркер появится в M3).
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/claude"
	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/embeddings"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/handlers"
	"github.com/interfin/interfin-ai-crm/internal/kanban"
	"github.com/interfin/interfin-ai-crm/internal/metrics"
	"github.com/interfin/interfin-ai-crm/internal/payment"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/rag"
	"github.com/interfin/interfin-ai-crm/internal/repo"
	"github.com/interfin/interfin-ai-crm/internal/server"
	"github.com/interfin/interfin-ai-crm/internal/telegram"
	"github.com/interfin/interfin-ai-crm/internal/worker"
	"github.com/interfin/interfin-ai-crm/internal/ws"
)

// redisPinger адаптирует redis.UniversalClient к server.Pinger.
type redisPinger struct{ client redis.UniversalClient }

func (p redisPinger) Ping(ctx context.Context) error {
	return p.client.Ping(ctx).Err()
}

// sqlPinger адаптирует *sql.DB (пул под GORM) к server.Pinger — readiness
// проверяет тот же пул, через который ходит приложение.
type sqlPinger struct{ db *sql.DB }

func (p sqlPinger) Ping(ctx context.Context) error {
	return p.db.PingContext(ctx)
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "config/config.yaml"
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err // config.Load уже даёт явную ошибку про недостающие переменные
	}
	log.Info("config loaded", "path", configPath, "port", cfg.Server.Port)

	// --- Postgres: единый пул GORM/pgx, ТОЛЬКО SimpleProtocol (CLAUDE.md §4.2) ---
	gormDB, err := db.Open(cfg.Database)
	if err != nil {
		return err
	}
	sqlDB, err := gormDB.DB()
	if err != nil {
		return fmt.Errorf("gorm sql db: %w", err)
	}
	defer func() {
		if err := sqlDB.Close(); err != nil {
			log.Warn("postgres close", "error", err)
		}
	}()

	leads, msgs, payments := repo.New(gormDB)
	knowledge, summaries, ragAudit := repo.NewRAG(gormDB)
	managers, refreshTokens := repo.NewAuth(gormDB)
	lgpdRepo := repo.NewLGPD(gormDB)

	// --- Redis: одиночный (dev) или Sentinel (prod), по конфигу ---
	var rdb redis.UniversalClient
	if len(cfg.Redis.SentinelAddrs) > 0 {
		rdb = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:    cfg.Redis.MasterName,
			SentinelAddrs: cfg.Redis.SentinelAddrs,
			Password:      cfg.Redis.Password,
		})
	} else {
		rdb = redis.NewClient(&redis.Options{
			Addr:     cfg.Redis.Addr,
			Password: cfg.Redis.Password,
		})
	}
	defer func() {
		if err := rdb.Close(); err != nil {
			log.Warn("redis close", "error", err)
		}
	}()

	// --- Asynq: клиент очереди process:inbound (обработчик — в M3) ---
	q := queue.NewClient(cfg.Redis)
	defer func() {
		if err := q.Close(); err != nil {
			log.Warn("queue close", "error", err)
		}
	}()

	// --- M11 §14: Prometheus — отдельный listener /metrics за IP-allowlist
	// (AQ²-10; снаружи периметра второй слой — Nginx mTLS, ops/nginx) +
	// поллер asynq_queue_size (dead letter для алерта §6.3).
	if cfg.Monitoring.PrometheusPort > 0 {
		metricsSrv, err := metrics.NewServer(
			cfg.Monitoring.PrometheusPort, cfg.Monitoring.MetricsIPAllowlist)
		if err != nil {
			return err
		}
		go func() {
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("metrics server", "error", err)
			}
		}()
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
				log.Warn("metrics server shutdown", "error", err)
			}
		}()

		statsCtx, statsCancel := context.WithCancel(context.Background())
		defer statsCancel()
		go metrics.NewQueueStats(queue.ConnOpt(cfg.Redis), metrics.QueueStatsInterval, log).Run(statsCtx)
		log.Info("metrics server started",
			"port", cfg.Monitoring.PrometheusPort,
			"allowlist", cfg.Monitoring.MetricsIPAllowlist)
	}

	// --- M11 §11.2: recovery-cron — вторая половина graceful degradation.
	// Вебхук при Redis down сохраняет inbound и ставит pending_task=TRUE;
	// этот cron раз в 30с перевыставляет задачи, когда Redis ожил.
	recoveryCtx, recoveryCancel := context.WithCancel(context.Background())
	defer recoveryCancel()
	go queue.NewRecovery(leads, q, queue.RecoveryInterval, log).Run(recoveryCtx)
	log.Info("pending_task recovery cron started", "interval", queue.RecoveryInterval.String())

	// --- Telegram: webhook-режим, никакого polling (CLAUDE.md §4.10) ---
	bot, err := telegram.NewWebhookBot(cfg.Telegram, log, false)
	if err != nil {
		return err
	}

	// --- M3: Asynq-воркер process:inbound (пайплайн §6.2, Claude §7) ---
	ai, err := claude.New(cfg.Claude)
	if err != nil {
		return err
	}
	budgeter, err := worker.NewBudgeter(cfg.Claude, ai)
	if err != nil {
		return err
	}
	sender := worker.NewTelebotSender(bot)

	// --- M4: RAG (Voyage + pgvector §7.1) и сводки диалогов (§7.3) ---
	embedder, err := embeddings.New(cfg.Embeddings)
	if err != nil {
		return err
	}
	retriever, err := rag.NewRetriever(cfg.RAG, embedder, knowledge, ragAudit, log)
	if err != nil {
		return err
	}
	summarizer := worker.NewSummarizer(leads, msgs, summaries, ai, worker.NewRedisLocker(rdb), log)

	// --- M5: state machine Kanban (§3) — TTL, anti-spam, crm:events ---
	ttlMgr := queue.NewTTLManager(cfg.Redis, leads,
		time.Duration(cfg.Kanban.TTLWarningHours)*time.Hour) // M9: ttl_warning
	defer func() {
		if err := ttlMgr.Close(); err != nil {
			log.Warn("ttl manager close", "error", err)
		}
	}()
	antiSpamMgr := queue.NewAntiSpamManager(cfg.Redis)
	defer func() {
		if err := antiSpamMgr.Close(); err != nil {
			log.Warn("antispam manager close", "error", err)
		}
	}()
	pub := events.NewRedisPublisher(rdb)
	machine := kanban.NewMachine(leads, msgs, ttlMgr, antiSpamMgr,
		pub, sender, cfg.Kanban, log)

	wrk := worker.New(
		cfg.Redis,
		worker.NewProcessor(worker.ProcessorDeps{
			Leads:         leads,
			Msgs:          msgs,
			Budgeter:      budgeter,
			AI:            ai,
			Sender:        sender,
			Retriever:     retriever,
			Summaries:     summaries,
			SummaryEnq:    q,
			SummaryEveryN: cfg.Kanban.SummaryEveryNMessages,
			Kanban:        machine,
			Log:           log,
		}),
		summarizer,
		sender,
		cfg.Telegram.ManagerChatID,
		log,
	)
	wrk.RegisterKanban(worker.NewKanbanHandlers(machine, log))
	// M8 §9.1: retention-cron — обработчик и суточный планировщик.
	wrk.RegisterLGPD(worker.NewLGPDHandlers(lgpdRepo, cfg.LGPD.RetentionDays, log))
	if err := wrk.Start(); err != nil {
		return err
	}
	// Останавливаем воркер до закрытия БД/Redis: Shutdown дожидается
	// активных задач, которым эти соединения ещё нужны.
	defer wrk.Shutdown()
	log.Info("asynq worker started", "task", queue.TypeProcessInbound)

	retention, err := queue.NewRetentionScheduler(cfg.Redis, log)
	if err != nil {
		return err
	}
	if err := retention.Start(); err != nil {
		return err
	}
	defer retention.Shutdown()
	log.Info("lgpd retention scheduler started",
		"task", queue.TypeLGPDRetention, "retention_days", cfg.LGPD.RetentionDays)
	// Регистрация webhook при старте (задача M2 §6). Не прошла — не стартуем:
	// без webhook приложение молча не получало бы ни одного апдейта.
	if err := telegram.RegisterWebhook(bot); err != nil {
		return err
	}
	log.Info("telegram webhook registered", "url", cfg.Telegram.WebhookURL)

	gin.SetMode(gin.ReleaseMode)
	router := server.New(server.Deps{
		Postgres: sqlPinger{db: sqlDB},
		Redis:    redisPinger{client: rdb},
	}, log)

	handlers.NewTelegramWebhook(leads, msgs, q, cfg.Telegram.WebhookSecret, log).
		Register(router, telegram.NewDispatcher(bot))

	// --- M7: аутентификация — JWT RS256 + refresh (§5.1) ---
	// Ключи ТОЛЬКО из файлов (Docker secrets, CLAUDE.md §4.9).
	privKey, pubKey, err := auth.LoadKeys(
		cfg.Auth.JWTPrivateKeyPath, cfg.Auth.JWTPublicKeyPath)
	if err != nil {
		return err
	}
	issuer := auth.NewIssuer(privKey, time.Duration(cfg.Auth.AccessTokenTTL)*time.Second)
	handlers.NewAuth(handlers.AuthDeps{
		Managers:   managers,
		Tokens:     refreshTokens,
		Issuer:     issuer,
		RefreshTTL: time.Duration(cfg.Auth.RefreshTokenTTL) * time.Second,
		Log:        log,
	}).Register(router)
	log.Info("auth endpoints registered",
		"access_ttl_sec", cfg.Auth.AccessTokenTTL, "refresh_ttl_sec", cfg.Auth.RefreshTokenTTL)

	// --- M6: платёжный вебхук CryptoBot (§3.3, §5.5) ---
	handlers.NewPaymentWebhook(handlers.PaymentWebhookDeps{
		Leads:        leads,
		Payments:     payments,
		Machine:      machine,
		Nonces:       payment.NewRedisNonceStore(rdb),
		Pub:          pub,
		Cfg:          cfg.Payment,
		TolerancePct: cfg.Kanban.UnderpaidTolerancePct,
		Log:          log,
	}).Register(router)
	log.Info("payment webhook registered",
		"gateway", cfg.Payment.Gateway, "testnet", cfg.Payment.UseTestnet)

	// --- M8: REST API менеджера (§4.1) за общим гейтом группы /api:
	// rate limit (§4.2, ДО auth — флуд не доходит до проверки подписи) →
	// JWT (M7) → роли §5.2 (admin не наследует manager — обе явно).
	api := router.Group("/api")
	if cfg.Server.RateLimitPerMin > 0 {
		api.Use(handlers.RateLimit(
			handlers.NewRedisRateLimiter(rdb, cfg.Server.RateLimitPerMin), log))
	}
	api.Use(
		auth.Middleware(auth.NewVerifier(pubKey)),
		auth.RequireRole(auth.RoleManager, auth.RoleAdmin),
	)
	handlers.NewLeads(handlers.LeadsDeps{
		Leads:    leads,
		Msgs:     msgs,
		Payments: payments,
		Machine:  machine,
		Log:      log,
	}).Register(api)
	handlers.NewLGPD(handlers.LGPDDeps{
		LGPD:     lgpdRepo,
		Msgs:     msgs,
		Payments: payments,
		Salt:     cfg.LGPD.ErasureSalt,
		Log:      log,
	}).Register(api)
	log.Info("rest api registered",
		"rate_limit_per_min", cfg.Server.RateLimitPerMin,
		"lgpd_retention_days", cfg.LGPD.RetentionDays)

	// --- M10: React-доска (§10) — собранная статика web/dist с того же
	// origin, что API (refresh-cookie и относительные URL). Не собрана —
	// сервер остаётся чистым API, dev-фронт живёт на Vite-прокси.
	server.ServeFrontend(router, cfg.Server.StaticDir, log)

	// --- M9: WebSocket real-time push (§10) — Hub на общем Redis-клиенте
	// (dev — single, prod — тот же Sentinel pool, §10/M11). Роут /ws/kanban
	// вне группы /api: auth — JWT из Sec-WebSocket-Protocol (§5.3).
	hub := ws.NewHub(ws.DefaultTimings, log)
	hubCtx, hubCancel := context.WithCancel(context.Background())
	defer hubCancel() // гасит подписку и отключает WS-клиентов при shutdown
	go hub.Run(hubCtx, rdb)
	ws.NewHandler(hub, auth.NewVerifier(pubKey), log).Register(router)
	log.Info("websocket hub started", "endpoint", "/ws/kanban")

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case sig := <-stop:
		log.Info("shutting down", "signal", sig.String())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	log.Info("server stopped")
	return nil
}
