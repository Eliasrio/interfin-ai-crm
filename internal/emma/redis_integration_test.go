// Интеграционные тесты EP-01 против реального Redis (REDIS_TEST_ADDR,
// иначе skip — как остальные интеграционные): настоящие TTL сессии и окна
// брутфорса, sliding-продление, PurgeAll.
package emma

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/interfin/interfin-ai-crm/internal/auth"
)

func testRedisStore(t *testing.T) (*RedisStore, redis.UniversalClient) {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR не задан — интеграционный тест пропущен")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	store := NewRedisStore(rdb)
	ctx := context.Background()
	if _, err := store.PurgeAll(ctx); err != nil {
		t.Fatalf("зачистка emma:pin:* перед тестом: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.PurgeAll(ctx)
		rdb.Close()
	})
	return store, rdb
}

func TestRedisStore_SessionTTLAndSliding(t *testing.T) {
	store, rdb := testRedisStore(t)
	ctx := context.Background()
	const id = "9001"

	if err := store.OpenSession(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ttl := rdb.TTL(ctx, sessionPrefix+id).Val(); ttl <= 29*time.Minute || ttl > SessionTTL {
		t.Fatalf("TTL сессии = %v, ждали ≈%v", ttl, SessionTTL)
	}

	// Sliding: старим сессию до 60с, Touch возвращает TTL к 30 минутам.
	if err := rdb.Expire(ctx, sessionPrefix+id, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	alive, err := store.TouchSession(ctx, id)
	if err != nil || !alive {
		t.Fatalf("touch: alive=%v err=%v", alive, err)
	}
	if ttl := rdb.TTL(ctx, sessionPrefix+id).Val(); ttl <= 29*time.Minute {
		t.Fatalf("после touch TTL = %v — продление не сработало", ttl)
	}

	// Закрытая сессия: touch=false, active=false.
	if err := store.CloseSession(ctx, id); err != nil {
		t.Fatal(err)
	}
	if alive, _ := store.TouchSession(ctx, id); alive {
		t.Fatal("touch на закрытой сессии вернул true")
	}
	if active, _ := store.SessionActive(ctx, id); active {
		t.Fatal("закрытая сессия числится активной")
	}
}

func TestRedisStore_FailWindow(t *testing.T) {
	store, rdb := testRedisStore(t)
	ctx := context.Background()
	const id = "9002"

	for i := 0; i < MaxFails; i++ {
		if err := store.IncrFail(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	count, retryAfter, err := store.FailState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if count != MaxFails {
		t.Fatalf("count = %d, ждали %d", count, MaxFails)
	}
	if retryAfter <= 0 || retryAfter > FailWindow {
		t.Fatalf("retry_after = %v, ждали (0, %v]", retryAfter, FailWindow)
	}
	// ExpireNX: повторный промах НЕ сдвигает окно (иначе перебор длил бы
	// блокировку самому себе — ок, но окно должно быть от ПЕРВОГО промаха).
	if err := rdb.Expire(ctx, failPrefix+id, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := store.IncrFail(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ttl := rdb.TTL(ctx, failPrefix+id).Val(); ttl > time.Minute {
		t.Fatalf("IncrFail сдвинул окно: TTL = %v", ttl)
	}

	if err := store.ResetFails(ctx, id); err != nil {
		t.Fatal(err)
	}
	if count, _, _ := store.FailState(ctx, id); count != 0 {
		t.Fatalf("после сброса count = %d", count)
	}
}

// TestBruteForceLockExpiry_LiveRedis — критерий приёмки 4 целиком на боевом
// RedisStore: 5 промахов → 429 c retry_after; верный PIN под блокировкой →
// 429; окно истекло (подмена TTL) → верный PIN проходит.
func TestBruteForceLockExpiry_LiveRedis(t *testing.T) {
	store, rdb := testRedisStore(t)
	rg := newRig(t, store)
	admin := rg.token(t, auth.RoleAdmin)
	setupPIN(t, rg, admin, "123456")
	ctx := context.Background()

	for i := 0; i < MaxFails; i++ {
		wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/verify", admin,
			gin.H{"pin": "000000"}), http.StatusUnauthorized, CodePinInvalid)
	}
	m := wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/verify", admin,
		gin.H{"pin": "123456"}), http.StatusTooManyRequests, CodePinLocked)
	if ra := m["retry_after"].(float64); ra <= 0 || ra > FailWindow.Seconds() {
		t.Fatalf("retry_after = %v, ждали (0, %v]", ra, FailWindow.Seconds())
	}

	// Подмена TTL: окно «истекает» через 1с вместо 15 минут (EXPIRE не
	// принимает доли секунды — go-redis округлил бы 100мс до 1с сам).
	if err := rdb.Expire(ctx, failPrefix+"1", time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/verify", admin,
		gin.H{"pin": "123456"}), http.StatusOK, "")
}

func TestRedisStore_PurgeAll(t *testing.T) {
	store, rdb := testRedisStore(t)
	ctx := context.Background()

	for _, id := range []string{"1", "2", "3"} {
		if err := store.OpenSession(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.IncrFail(ctx, "1"); err != nil {
		t.Fatal(err)
	}

	purged, err := store.PurgeAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 4 {
		t.Fatalf("purged = %d, ждали 4 (3 сессии + 1 счётчик)", purged)
	}
	if n := rdb.Exists(ctx, sessionPrefix+"1", sessionPrefix+"2",
		sessionPrefix+"3", failPrefix+"1").Val(); n != 0 {
		t.Fatalf("после PurgeAll осталось %d ключей", n)
	}
}
