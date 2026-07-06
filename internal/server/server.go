// Package server — HTTP-каркас (Gin): liveness/readiness эндпоинты M0.
// Бизнес-логики здесь нет и не будет — только инфраструктурные ручки.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/metrics"
)

// Pinger — минимальный контракт зависимости для readiness-проверки.
// Ему удовлетворяют *pgxpool.Pool и адаптер Redis (см. cmd/server).
type Pinger interface {
	Ping(ctx context.Context) error
}

// readyCheckTimeout — сколько ждём каждую зависимость, прежде чем счесть
// её недоступной. Держим маленьким: readiness дёргают часто.
const readyCheckTimeout = 2 * time.Second

// Deps — зависимости, состояние которых определяет readiness.
type Deps struct {
	Postgres Pinger
	Redis    Pinger
}

// New собирает Gin-роутер с GET /health и GET /ready.
func New(deps Deps, log *slog.Logger) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	// M11 §14: gin_request_duration_seconds на все роуты (включая вебхук —
	// его латентность питает алерт WebhookHighLatency).
	r.Use(metrics.Gin())

	// Liveness: процесс жив и умеет отвечать. Ничего внешнего не проверяем.
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Readiness: живы обе зависимости — 200, иначе 503 (критерий приёмки M0).
	r.GET("/ready", func(c *gin.Context) {
		failed := map[string]string{}
		for name, dep := range map[string]Pinger{
			"postgres": deps.Postgres,
			"redis":    deps.Redis,
		} {
			ctx, cancel := context.WithTimeout(c.Request.Context(), readyCheckTimeout)
			err := dep.Ping(ctx)
			cancel()
			if err != nil {
				failed[name] = err.Error()
				log.Warn("readiness check failed", "dependency", name, "error", err)
			}
		}

		if len(failed) > 0 {
			// Формат ошибки — по соглашению CLAUDE.md §5.
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "dependencies not ready",
				"code":  "ERR_NOT_READY",
				"deps":  failed,
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	return r
}
