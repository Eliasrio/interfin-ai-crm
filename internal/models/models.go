// Package models — GORM-модели, зеркалящие схему БД (SRS §8).
//
// Правила пакета (AQ²-fix #1, CLAUDE.md §4.1):
//   - каждое персистентное поле обязано иметь явный тег gorm:"column:<имя>";
//   - имя колонки обязано существовать в migrations/*.up.sql;
//   - обе инварианты проверяет cmd/schema-lint (запускается в CI);
//   - новая модель ОБЯЗАНА быть добавлена в реестр All() — иначе schema-lint
//     её не увидит.
//
// Nullable-колонки отражены указателями, NOT NULL — значениями.
package models

import (
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// JSONB — сырой JSON для колонок jsonb. Отдельный тип обязателен: голый
// []byte pgx кодирует как bytea, и вставка в jsonb падает.
type JSONB []byte

func (j JSONB) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	return string(j), nil
}

func (j *JSONB) Scan(src interface{}) error {
	switch v := src.(type) {
	case nil:
		*j = nil
	case []byte:
		*j = append((*j)[:0], v...)
	case string:
		*j = JSONB(v)
	default:
		return fmt.Errorf("models: JSONB: неожиданный тип %T", src)
	}
	return nil
}

// Направления сообщений (CHECK-констрейнт в §8.2).
const (
	DirectionInbound  = "inbound"
	DirectionOutbound = "outbound"
)

// Lead — §8.1. Строка Kanban-доски: один Telegram-пользователь = один лид.
type Lead struct {
	ID               int64          `gorm:"column:id;primaryKey"`
	TelegramUserID   int64          `gorm:"column:telegram_user_id"` // UNIQUE NOT NULL; при erasure хешируется, не NULL (CLAUDE.md §4.8)
	Name             *string        `gorm:"column:name"`
	Phone            *string        `gorm:"column:phone"`
	TgUsername       *string        `gorm:"column:tg_username"`
	StageID          int16          `gorm:"column:stage_id"`
	MessageCount     int            `gorm:"column:message_count"`   // только inbound (CLAUDE.md §4.3)
	AntiSpamCount    int            `gorm:"column:anti_spam_count"` // per-stage inbound, сбрасывается при смене стадии (M5)
	LastActivityAt   time.Time      `gorm:"column:last_activity_at"`
	ManualResolution bool           `gorm:"column:manual_resolution"`
	TTLTaskID        *string        `gorm:"column:ttl_task_id"`
	PendingTask      bool           `gorm:"column:pending_task"` // AQ²-fix #1: Redis down → TRUE
	EscalatedAt      *time.Time     `gorm:"column:escalated_at"` // AQ²-fix #8
	ConsentGivenAt   *time.Time     `gorm:"column:consent_given_at"`
	DeletedAt        gorm.DeletedAt `gorm:"column:deleted_at"` // LGPD erasure = soft delete; выборки сами исключают стёртых
	CreatedAt        time.Time      `gorm:"column:created_at"`
}

func (Lead) TableName() string { return "leads" }

// Message — §8.2. Одна реплика диалога (лид или бот).
type Message struct {
	ID        int64     `gorm:"column:id;primaryKey"`
	LeadID    int64     `gorm:"column:lead_id"`
	Direction string    `gorm:"column:direction"` // inbound | outbound (CHECK в БД)
	Content   string    `gorm:"column:content"`
	Tokens    *int      `gorm:"column:tokens"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (Message) TableName() string { return "messages" }

// PaymentEvent — §8.3. НЕ удаляется при LGPD erasure:
// фискальная retention 5 лет (AQ²-fix #4, CLAUDE.md §4.8).
type PaymentEvent struct {
	ID             int64               `gorm:"column:id;primaryKey"`
	LeadID         int64               `gorm:"column:lead_id"`
	Gateway        *string             `gorm:"column:gateway"`
	Status         *string             `gorm:"column:status"`
	AmountDue      decimal.NullDecimal `gorm:"column:amount_due"` // NUMERIC(20,8): decimal, не float — крипто-суммы
	AmountReceived decimal.NullDecimal `gorm:"column:amount_received"`
	NetReceived    decimal.NullDecimal `gorm:"column:net_received"`
	Currency       *string             `gorm:"column:currency"`
	ToleranceOk    *bool               `gorm:"column:tolerance_ok"` // underpaid ≤ 2% (§8.5)
	RawPayload     JSONB               `gorm:"column:raw_payload"`
	CreatedAt      time.Time           `gorm:"column:created_at"`
}

func (PaymentEvent) TableName() string { return "payment_events" }

// RagAudit — §8.4. След каждого RAG-запроса; results=0 фиксирует rag_miss (M4).
// lead_id без FK: аудит переживает удаление лида retention-cron'ом.
type RagAudit struct {
	ID        int64     `gorm:"column:id;primaryKey"`
	LeadID    *int64    `gorm:"column:lead_id"`
	QueryText string    `gorm:"column:query_text"`
	Results   int       `gorm:"column:results"`
	Threshold float64   `gorm:"column:threshold"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (RagAudit) TableName() string { return "rag_audit" }

// LGPDAudit — §8.4/§9.2. Кто, что и откуда сделал с персональными данными.
type LGPDAudit struct {
	ID          int64     `gorm:"column:id;primaryKey"`
	Action      string    `gorm:"column:action"`
	LeadID      *int64    `gorm:"column:lead_id"`
	PerformedBy string    `gorm:"column:performed_by"`
	IPAddress   *string   `gorm:"column:ip_address"`
	CreatedAt   time.Time `gorm:"column:created_at"`
}

func (LGPDAudit) TableName() string { return "lgpd_audit" }

// All — реестр всех персистентных моделей для cmd/schema-lint.
// Добавил модель — добавь её сюда, иначе lint её не проверит.
func All() []interface{} {
	return []interface{}{
		Lead{},
		Message{},
		PaymentEvent{},
		RagAudit{},
		LGPDAudit{},
	}
}
