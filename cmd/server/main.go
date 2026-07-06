// cmd/server — точка входа HTTP-сервера Interfin AI-CRM.
//
// M0: загрузка конфига, подключение Postgres (pgx SimpleProtocol, AQ²-fix #3)
// и Redis, эндпоинты /health и /ready, graceful shutdown.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/server"
)

// redisPinger адаптирует redis.UniversalClient к server.Pinger.
type redisPinger struct{ client redis.UniversalClient }

func (p redisPinger) Ping(ctx context.Context) error {
	return p.client.Ping(ctx).Err()
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

	// --- Postgres: pgx ТОЛЬКО в SimpleProtocol (CLAUDE.md §4.2) ---
	poolCfg, err := pgxpool.ParseConfig(cfg.Database.DSN)
	if err != nil {
		return fmt.Errorf("parse postgres dsn: %w", err)
	}
	poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	poolCfg.MaxConns = int32(cfg.Database.MaxOpenConns)

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return fmt.Errorf("create postgres pool: %w", err)
	}
	defer pool.Close()

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

	gin.SetMode(gin.ReleaseMode)
	router := server.New(server.Deps{
		Postgres: pool,
		Redis:    redisPinger{client: rdb},
	}, log)

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
