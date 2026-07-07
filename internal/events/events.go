// Package events — real-time события CRM через Redis pub/sub (SRS §10.1).
//
// Контракт M5 → M9: каждая смена стадии и anti-spam-событие уходят
// PUBLISH'ем в канал crm:events; WS Hub (M9) подписан на канал и
// broadcast'ит авторизованным клиентам.
//
// ПРИМЕЧАНИЕ к схеме payload: task-файлы M5/M9 ссылаются на SRS §4.3
// (WS events schema), но в SRS v2.3 раздела 4.3 нет — нумерация §4
// обрывается на 4.2. Типы событий известны из M9 (stage_change,
// antispam_alert, payment_received, ttl_warning) и §3.5
// (manager_escalation); структура Event ниже — каноническое определение
// схемы, M6/M9 обязаны использовать её, а не выдумывать свою.
//
// Redis pub/sub — fire-and-forget: потерянное событие не ретраится,
// пропуски клиент добирает catch-up'ом GET /api/leads?updated_since
// (§10.3). Поэтому ошибка Publish логируется, но не роняет переход.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

// Channel — канал Redis pub/sub для всех real-time событий CRM (§10.1).
const Channel = "crm:events"

// Типы событий (M9 + §3.5).
const (
	TypeStageChange       = "stage_change"
	TypeAntiSpamAlert     = "antispam_alert"     // §3.5: лимит 25 inbound достигнут
	TypeManagerEscalation = "manager_escalation" // AQ²-fix #8: 48ч молчания
	TypePaymentReceived   = "payment_received"   // M6 §3.3: платёж принят; tolerance решил стадию
	TypeTTLWarning        = "ttl_warning"        // M9: до истечения TTL стадии осталось < kanban.ttl_warning_hours
	TypeMessage           = "message"            // M12: новая строка в messages (чат живой в обе стороны)
)

// Event — единица канала crm:events. Одна структура на все типы:
// потребителю (React) проще матчить по type, чем по форме объекта.
type Event struct {
	Type    string `json:"type"`
	LeadID  int64  `json:"lead_id"`
	StageID int16  `json:"stage_id"` // стадия ПОСЛЕ события

	// Только для stage_change:
	OldStageID *int16 `json:"old_stage_id,omitempty"`
	Actor      string `json:"actor,omitempty"` // system | manager | payment | ttl

	// Только для antispam_alert:
	AntiSpamCount int `json:"anti_spam_count,omitempty"`

	// Только для payment_received (M6): net_received строкой — decimal
	// уходит клиенту без потерь точности float.
	Amount      string `json:"amount,omitempty"`
	Currency    string `json:"currency,omitempty"`
	ToleranceOk *bool  `json:"tolerance_ok,omitempty"`

	// Только для message (M12). Полный текст допустим: канал внутренний
	// (Redis за паролем, WS за JWT). Событие НЕ трогает stage — StageID выше
	// заполняется ТЕКУЩЕЙ стадией лида (консистентность карточки на фронте).
	// Author: '' — лид (inbound), 'bot' — Эмма, 'manager:<id>' — менеджер.
	Direction string `json:"direction,omitempty"`
	Author    string `json:"author,omitempty"`
	Content   string `json:"content,omitempty"`

	Reason string    `json:"reason,omitempty"` // человекочитаемый триггер (лог/отладка)
	TS     time.Time `json:"ts"`               // клиент хранит как last_event_ts для catch-up §10.3
}

// MessageEvent собирает событие message из строки messages (M12) — единая
// точка для всех трёх публикаторов (ingestion, воркер, ручка менеджера).
// stageID — текущая стадия лида. TS из created_at строки; свежая вставка
// без отметки — штамп поставит Publish.
func MessageEvent(m *models.Message, stageID int16) Event {
	author := ""
	if m.Author != nil {
		author = *m.Author
	}
	return Event{
		Type:      TypeMessage,
		LeadID:    m.LeadID,
		StageID:   stageID,
		Direction: m.Direction,
		Author:    author,
		Content:   m.Content,
		TS:        m.CreatedAt,
	}
}

// Publisher — контракт публикации для бизнес-логики (в тестах — фейк).
type Publisher interface {
	Publish(ctx context.Context, ev Event) error
}

// RedisPublisher — боевой Publisher поверх go-redis (single или Sentinel —
// клиент передаётся снаружи, тот же, что для readiness в cmd/server).
type RedisPublisher struct {
	rdb redis.UniversalClient
}

func NewRedisPublisher(rdb redis.UniversalClient) *RedisPublisher {
	return &RedisPublisher{rdb: rdb}
}

func (p *RedisPublisher) Publish(ctx context.Context, ev Event) error {
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("events: marshal %s: %w", ev.Type, err)
	}
	if err := p.rdb.Publish(ctx, Channel, payload).Err(); err != nil {
		return fmt.Errorf("events: publish %s: %w", ev.Type, err)
	}
	return nil
}
