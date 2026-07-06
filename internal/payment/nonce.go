// nonce.go — replay-защита §5.5, слой nonce: одноразовость update_id вебхука.
//
// Claim атомарен (SET NX EX): первый запрос с данным nonce забирает его,
// повтор в течение TTL — replay → 403. TTL = 2× replay-окна: более старый
// повтор отсекает проверка timestamp, хранить дольше незачем.
//
// Release возвращает nonce при провале обработки: шлюз повторяет доставку
// до первого 2xx, и его ретрай не должен быть отвергнут как replay.
// Идемпотентность самого ретрая — на уникальном индексе payment_events
// (0008) и ветке «уже в to» машины стадий (M5).
package payment

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// NonceStore — контракт replay-защиты для хендлера (в тестах — фейк).
type NonceStore interface {
	// Claim пытается занять nonce на ttl. false — nonce уже занят (replay).
	Claim(ctx context.Context, nonce string, ttl time.Duration) (bool, error)
	// Release освобождает nonce (провал обработки → пускаем ретрай шлюза).
	Release(ctx context.Context, nonce string) error
}

const noncePrefix = "payment:nonce:"

// RedisNonceStore — боевой NonceStore поверх go-redis (single или Sentinel).
type RedisNonceStore struct{ rdb redis.UniversalClient }

func NewRedisNonceStore(rdb redis.UniversalClient) *RedisNonceStore {
	return &RedisNonceStore{rdb: rdb}
}

func (s *RedisNonceStore) Claim(ctx context.Context, nonce string, ttl time.Duration) (bool, error) {
	ok, err := s.rdb.SetNX(ctx, noncePrefix+nonce, 1, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("payment: nonce claim: %w", err)
	}
	return ok, nil
}

func (s *RedisNonceStore) Release(ctx context.Context, nonce string) error {
	if err := s.rdb.Del(ctx, noncePrefix+nonce).Err(); err != nil {
		return fmt.Errorf("payment: nonce release: %w", err)
	}
	return nil
}
