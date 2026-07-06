// ratelimit.go — rate limit §4.2: 100 запросов в минуту с одного IP на
// /api/*, сверх лимита — 429 ERR_RATE_LIMITED.
//
// Счётчик — в Redis (окно фиксируется первым запросом IP): лимит общий на
// все реплики приложения, а не per-process. Redis недоступен — fail-open:
// API продолжает отвечать без лимита (graceful degradation в духе §11.2;
// закрыть API целиком из-за упавшего Redis хуже, чем минуту не лимитировать).
package handlers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// RateLimiter — контракт лимитера для middleware (в тестах — фейк).
type RateLimiter interface {
	// Allow — пропустить ли очередной запрос от key (IP).
	Allow(ctx context.Context, key string) (bool, error)
}

// RedisRateLimiter — фиксированное окно на INCR+EXPIRE: первый запрос IP
// открывает минутное окно, 101-й в том же окне получает 429 (критерий M8).
type RedisRateLimiter struct {
	rdb    redis.UniversalClient
	limit  int
	window time.Duration
}

func NewRedisRateLimiter(rdb redis.UniversalClient, perMinute int) *RedisRateLimiter {
	return &RedisRateLimiter{rdb: rdb, limit: perMinute, window: time.Minute}
}

func (l *RedisRateLimiter) Allow(ctx context.Context, key string) (bool, error) {
	rkey := "ratelimit:" + key
	n, err := l.rdb.Incr(ctx, rkey).Result()
	if err != nil {
		return false, fmt.Errorf("ratelimit: incr %s: %w", rkey, err)
	}
	// ExpireNX, не «EXPIRE при n==1»: ставит TTL только если его нет, и
	// заодно лечит ключ, оставшийся без TTL после сбоя между INCR и EXPIRE
	// (иначе такой ключ копил бы счётчик вечно — перманентный 429).
	if err := l.rdb.ExpireNX(ctx, rkey, l.window).Err(); err != nil {
		return false, fmt.Errorf("ratelimit: expire %s: %w", rkey, err)
	}
	return n <= int64(l.limit), nil
}

// RateLimit — gin-middleware поверх лимитера. Ставится ПЕРВЫМ в цепочке
// /api (до auth): флуд не должен доходить даже до проверки подписи JWT.
func RateLimit(l RateLimiter, log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		ok, err := l.Allow(c.Request.Context(), c.ClientIP())
		if err != nil {
			log.Warn("ratelimit: лимитер недоступен, запрос пропущен без лимита",
				"ip", c.ClientIP(), "error", err)
			c.Next()
			return
		}
		if !ok {
			apiError(c, http.StatusTooManyRequests,
				"слишком много запросов, лимит 100/мин с IP", codeRateLimited)
			return
		}
		c.Next()
	}
}
