// Интеграционный тест RedisRateLimiter против реального Redis:
// критерий приёмки M8 — лимит срабатывает ровно на 101-м запросе за минуту.
// Запуск: REDIS_TEST_ADDR=localhost:6379 go test ./internal/handlers
// Без REDIS_TEST_ADDR — skip.
package handlers

import (
	"context"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestRedisRateLimiter101st(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR не задан — интеграционный тест пропущен")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	ctx := context.Background()

	const ip = "203.0.113.7" // TEST-NET-3: с боевыми ключами не пересечётся
	if err := rdb.Del(ctx, "ratelimit:"+ip).Err(); err != nil {
		t.Fatal(err)
	}

	l := NewRedisRateLimiter(rdb, 100)
	for i := 1; i <= 100; i++ {
		ok, err := l.Allow(ctx, ip)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("запрос %d отклонён до лимита", i)
		}
	}
	ok, err := l.Allow(ctx, ip)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("101-й запрос за минуту пропущен — лимит §4.2 не сработал")
	}

	// Окно имеет TTL — счётчик не вечный (иначе перманентный 429).
	ttl, err := rdb.TTL(ctx, "ratelimit:"+ip).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 {
		t.Fatalf("у ключа лимита нет TTL: %v", ttl)
	}
}
