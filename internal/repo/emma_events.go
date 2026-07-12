// emma_events.go — журнал работы Эммы (таблица 0020). EP-04 пишет
// file_sent и error (file_not_found/telegram_api), EP-06 добавит reply
// и построит поверх статистику вкладки 6.
package repo

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

// NewEmmaEvents — репозиторий emma_events поверх того же *gorm.DB.
func NewEmmaEvents(db *gorm.DB) EmmaEventsRepo {
	return &emmaEventsRepo{db: db}
}

type emmaEventsRepo struct{ db *gorm.DB }

func (r *emmaEventsRepo) Create(ctx context.Context, ev *models.EmmaEvent) error {
	if err := r.db.WithContext(ctx).Create(ev).Error; err != nil {
		return fmt.Errorf("repo: create emma event %s: %w", ev.EventType, err)
	}
	return nil
}
