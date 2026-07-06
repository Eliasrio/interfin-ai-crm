package payment

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Интеграционный тест против реального Redis (REDIS_TEST_ADDR, в CI задан):
// атомарность SET NX EX — фундамент replay-защиты §5.5.
func TestRedisNonceStore(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR не задан — интеграционный тест пропущен")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { rdb.Close() })
	ctx := context.Background()

	store := NewRedisNonceStore(rdb)
	nonce := "test-" + t.Name()
	t.Cleanup(func() { rdb.Del(ctx, noncePrefix+nonce) })

	fresh, err := store.Claim(ctx, nonce, 10*time.Minute)
	if err != nil || !fresh {
		t.Fatalf("первый Claim: fresh=%v err=%v", fresh, err)
	}
	// Повтор — replay.
	fresh, err = store.Claim(ctx, nonce, 10*time.Minute)
	if err != nil || fresh {
		t.Fatalf("повторный Claim обязан вернуть false: fresh=%v err=%v", fresh, err)
	}
	// TTL взведён — nonce не живёт вечно.
	if ttl := rdb.TTL(ctx, noncePrefix+nonce).Val(); ttl <= 0 || ttl > 10*time.Minute {
		t.Errorf("TTL nonce = %v, ожидали (0, 10м]", ttl)
	}
	// Release возвращает nonce (провал обработки → ретрай шлюза проходит).
	if err := store.Release(ctx, nonce); err != nil {
		t.Fatal(err)
	}
	fresh, err = store.Claim(ctx, nonce, 10*time.Minute)
	if err != nil || !fresh {
		t.Fatalf("Claim после Release: fresh=%v err=%v", fresh, err)
	}
}
