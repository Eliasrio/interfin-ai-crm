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

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/handlers"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/repo"
	"github.com/interfin/interfin-ai-crm/internal/server"
	"github.com/interfin/interfin-ai-crm/internal/telegram"
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

	leads, msgs, _ := repo.New(gormDB)

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

	// --- Telegram: webhook-режим, никакого polling (CLAUDE.md §4.10) ---
	bot, err := telegram.NewWebhookBot(cfg.Telegram, log, false)
	if err != nil {
		return err
	}
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
