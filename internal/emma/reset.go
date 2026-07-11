// reset.go — сброс забытого PIN (EP-01 §7, ТЗ §2.2): только с сервера,
// через one-off cmd/reset-emma-pin. Ядро вынесено сюда — команда остаётся
// тонкой обёрткой, а логика покрывается тестами пакета.
package emma

import (
	"context"
	"fmt"

	"github.com/interfin/interfin-ai-crm/internal/settings"
)

// Purger — сметает все PIN-ключи Redis (боевой — RedisStore.PurgeAll).
type Purger interface {
	PurgeAll(ctx context.Context) (int64, error)
}

// Reset очищает emma_panel.pin_hash (пустая строка = «PIN не задан»,
// bootstrap открывается заново) и удаляет все PIN-сессии и счётчики
// брутфорса. Возвращает число удалённых Redis-ключей.
func Reset(ctx context.Context, set Settings, purger Purger) (int64, error) {
	if err := set.SetString(ctx, settings.KeyPinHash, ""); err != nil {
		return 0, fmt.Errorf("emma: reset: очистка pin_hash: %w", err)
	}
	purged, err := purger.PurgeAll(ctx)
	if err != nil {
		return purged, fmt.Errorf("emma: reset: очистка redis: %w", err)
	}
	return purged, nil
}
