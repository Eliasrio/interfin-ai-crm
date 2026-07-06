// Package repo — repository-слой над GORM (контракт M1 наружу).
//
// Интерфейсы LeadRepo / MessageRepo / PaymentRepo — единственная точка доступа
// бизнес-логики (M2+) к таблицам §8. Прямые запросы GORM вне этого пакета
// запрещены: инварианты вроде «message_count считает только inbound»
// (CLAUDE.md §4.3) живут именно здесь.
package repo

import (
	"context"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

// ErrNotFound возвращают Get*-методы, когда записи нет.
// Обёртка над gorm.ErrRecordNotFound, чтобы вызывающие не зависели от GORM.
var ErrNotFound = errNotFound{}

type errNotFound struct{}

func (errNotFound) Error() string { return "repo: запись не найдена" }

// LeadRepo — leads (§8.1).
type LeadRepo interface {
	// Create вставляет нового лида; заполняет lead.ID.
	Create(ctx context.Context, lead *models.Lead) error
	// GetByID возвращает лида по PK. ErrNotFound, если нет или стёрт (deleted_at).
	GetByID(ctx context.Context, id int64) (*models.Lead, error)
	// GetByTelegramUserID — основная выборка ingestion-пайплайна (M2).
	GetByTelegramUserID(ctx context.Context, tgUserID int64) (*models.Lead, error)
	// Save перезаписывает все поля лида (lead.ID обязан быть заполнен).
	Save(ctx context.Context, lead *models.Lead) error
	// UpdateFields точечно меняет колонки по PK, например
	// {"pending_task": true} при Redis down (§11.2).
	UpdateFields(ctx context.Context, id int64, fields map[string]interface{}) error
	// TransitionStage атомарно (CAS: WHERE stage_id = from) переводит лида
	// в стадию to, сбрасывает anti_spam_count (§3.5) и обновляет
	// last_activity_at — точку отсчёта TTL новой стадии (CLAUDE.md §4.7).
	// false — лид уже не в from: конкурирующий переход победил
	// (Manual/Payment переопределяют авто-триггеры, §3.1) — вызывающий
	// обязан НЕ применять свой переход.
	TransitionStage(ctx context.Context, id int64, from, to int16) (bool, error)
}

// MessageRepo — messages (§8.2).
type MessageRepo interface {
	// CreateInbound атомарно (одна транзакция): вставляет сообщение
	// direction='inbound' и у лида message_count+1, anti_spam_count+1,
	// last_activity_at=NOW(). Только inbound двигает счётчики (CLAUDE.md §4.3).
	CreateInbound(ctx context.Context, msg *models.Message) error
	// CreateOutbound вставляет ответ бота/менеджера. Счётчики НЕ трогает.
	CreateOutbound(ctx context.Context, msg *models.Message) error
	// ListByLead — последние limit сообщений лида, от старых к новым
	// (порядок, в котором история уходит в контекст Claude).
	ListByLead(ctx context.Context, leadID int64, limit int) ([]models.Message, error)
}

// PaymentRepo — payment_events (§8.3).
type PaymentRepo interface {
	// Create вставляет событие платёжного gateway. Идемпотентен по
	// (gateway, raw_payload->>'update_id') — повтор вебхука не плодит
	// вторую фискальную запись (индекс 0008, M6).
	Create(ctx context.Context, ev *models.PaymentEvent) error
	// ListByLead — все события лида, от старых к новым.
	ListByLead(ctx context.Context, leadID int64) ([]models.PaymentEvent, error)
}
