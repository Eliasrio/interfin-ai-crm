// Package emma — контур PIN-защиты панели управления Эммой (EP-01,
// ТЗ EMMA_PANEL_TZ_v2 §2): bcrypt-PIN в settings, сессия и счётчик
// брутфорса в Redis, middleware «admin + PIN-сессия» для /api/emma/*.
//
// Redis здесь FAIL-CLOSED (§2.2 ТЗ): любая ошибка Redis наружу — 503,
// панель закрыта. Это противоположно rate-limit M8 (fail-open): там
// деградация — пустить, здесь — не пустить.
package emma

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Ключи Redis. Оба под общим префиксом emma:pin: — cmd/reset-emma-pin
// сметает их одним SCAN'ом.
const (
	sessionPrefix = "emma:pin:"      // emma:pin:<manager_id> = "1"
	failPrefix    = "emma:pin:fail:" // emma:pin:fail:<manager_id> = счётчик
)

// Параметры контура (ТЗ §2.2).
const (
	// SessionTTL — жизнь PIN-сессии; каждый запрос к защищённой группе
	// продлевает (sliding).
	SessionTTL = 30 * time.Minute
	// MaxFails — неверных попыток до блокировки.
	MaxFails = 5
	// FailWindow — TTL счётчика попыток = длительность блокировки.
	FailWindow = 15 * time.Minute
)

// Store — контракт Redis-контура PIN (в тестах — in-memory фейк и
// «сломанный» стор для fail-closed веток).
type Store interface {
	// OpenSession открывает/перезаводит сессию менеджера на SessionTTL.
	OpenSession(ctx context.Context, managerID string) error
	// TouchSession продлевает сессию (sliding). false — сессии нет.
	TouchSession(ctx context.Context, managerID string) (bool, error)
	// SessionActive — есть ли сессия (без продления, для pin/status).
	SessionActive(ctx context.Context, managerID string) (bool, error)
	// CloseSession закрывает сессию (выход из панели).
	CloseSession(ctx context.Context, managerID string) error
	// FailState — счётчик неверных попыток и оставшееся время блокировки.
	FailState(ctx context.Context, managerID string) (count int64, retryAfter time.Duration, err error)
	// IncrFail — +1 к счётчику, окно взводится один раз (ExpireNX).
	IncrFail(ctx context.Context, managerID string) error
	// ResetFails сбрасывает счётчик (успешная проверка PIN).
	ResetFails(ctx context.Context, managerID string) error
}

// RedisStore — боевой Store поверх go-redis.
type RedisStore struct {
	rdb redis.UniversalClient
}

func NewRedisStore(rdb redis.UniversalClient) *RedisStore {
	return &RedisStore{rdb: rdb}
}

func (s *RedisStore) OpenSession(ctx context.Context, managerID string) error {
	if err := s.rdb.Set(ctx, sessionPrefix+managerID, "1", SessionTTL).Err(); err != nil {
		return fmt.Errorf("emma: open session: %w", err)
	}
	return nil
}

// TouchSession — одним EXPIRE: он и проверяет существование (false на
// отсутствующем ключе), и продлевает TTL.
func (s *RedisStore) TouchSession(ctx context.Context, managerID string) (bool, error) {
	ok, err := s.rdb.Expire(ctx, sessionPrefix+managerID, SessionTTL).Result()
	if err != nil {
		return false, fmt.Errorf("emma: touch session: %w", err)
	}
	return ok, nil
}

func (s *RedisStore) SessionActive(ctx context.Context, managerID string) (bool, error) {
	n, err := s.rdb.Exists(ctx, sessionPrefix+managerID).Result()
	if err != nil {
		return false, fmt.Errorf("emma: session exists: %w", err)
	}
	return n > 0, nil
}

func (s *RedisStore) CloseSession(ctx context.Context, managerID string) error {
	if err := s.rdb.Del(ctx, sessionPrefix+managerID).Err(); err != nil {
		return fmt.Errorf("emma: close session: %w", err)
	}
	return nil
}

func (s *RedisStore) FailState(ctx context.Context, managerID string) (int64, time.Duration, error) {
	key := failPrefix + managerID
	count, err := s.rdb.Get(ctx, key).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("emma: fail count: %w", err)
	}
	ttl, err := s.rdb.TTL(ctx, key).Result()
	if err != nil {
		return 0, 0, fmt.Errorf("emma: fail ttl: %w", err)
	}
	if ttl < 0 {
		ttl = 0
	}
	return count, ttl, nil
}

// IncrFail — INCR + ExpireNX (паттерн rate-limit M8): окно 15 минут
// взводится первым промахом; ExpireNX заодно лечит ключ, оставшийся без
// TTL после сбоя между INCR и EXPIRE.
func (s *RedisStore) IncrFail(ctx context.Context, managerID string) error {
	key := failPrefix + managerID
	if err := s.rdb.Incr(ctx, key).Err(); err != nil {
		return fmt.Errorf("emma: incr fail: %w", err)
	}
	if err := s.rdb.ExpireNX(ctx, key, FailWindow).Err(); err != nil {
		return fmt.Errorf("emma: expire fail: %w", err)
	}
	return nil
}

func (s *RedisStore) ResetFails(ctx context.Context, managerID string) error {
	if err := s.rdb.Del(ctx, failPrefix+managerID).Err(); err != nil {
		return fmt.Errorf("emma: reset fails: %w", err)
	}
	return nil
}

// PurgeAll удаляет ВСЕ PIN-сессии и счётчики (cmd/reset-emma-pin).
// SCAN, не KEYS — не блокирует Redis на большом keyspace.
func (s *RedisStore) PurgeAll(ctx context.Context) (int64, error) {
	var deleted int64
	iter := s.rdb.Scan(ctx, 0, sessionPrefix+"*", 100).Iterator()
	for iter.Next(ctx) {
		n, err := s.rdb.Del(ctx, iter.Val()).Result()
		if err != nil {
			return deleted, fmt.Errorf("emma: purge del %s: %w", iter.Val(), err)
		}
		deleted += n
	}
	if err := iter.Err(); err != nil {
		return deleted, fmt.Errorf("emma: purge scan: %w", err)
	}
	return deleted, nil
}
