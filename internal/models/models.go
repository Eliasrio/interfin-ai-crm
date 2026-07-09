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
	"strconv"
	"strings"
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

// Vector — колонка pgvector (vector(1024), §7.1). Отдельный тип обязателен:
// в SimpleProtocol pgx не знает тип vector, поэтому значение ходит строковым
// литералом pgvector "[0.1,0.2,...]" в обе стороны.
type Vector []float32

func (v Vector) Value() (driver.Value, error) {
	if len(v) == 0 {
		return nil, nil
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String(), nil
}

func (v *Vector) Scan(src interface{}) error {
	var s string
	switch raw := src.(type) {
	case nil:
		*v = nil
		return nil
	case []byte:
		s = string(raw)
	case string:
		s = raw
	default:
		return fmt.Errorf("models: Vector: неожиданный тип %T", src)
	}
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return fmt.Errorf("models: Vector: не литерал pgvector: %.32q", s)
	}
	body := strings.Trim(s, "[]")
	if body == "" {
		*v = Vector{}
		return nil
	}
	parts := strings.Split(body, ",")
	out := make(Vector, 0, len(parts))
	for _, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return fmt.Errorf("models: Vector: компонент %q: %w", p, err)
		}
		out = append(out, float32(f))
	}
	*v = out
	return nil
}

// Направления сообщений (CHECK-констрейнт в §8.2).
const (
	DirectionInbound  = "inbound"
	DirectionOutbound = "outbound"
)

// Авторство outbound-сообщений (M12): бот либо конкретный менеджер.
// NULL в старых строках читается как AuthorBot (0012); у inbound автор
// всегда NULL — автор и так лид.
const (
	AuthorBot           = "bot"
	AuthorManagerPrefix = "manager:" // + Subject из JWT claims
)

// Режимы диалога лида (M13, CHECK-констрейнт в 0013): кто ведёт переписку.
const (
	DialogModeBot   = "bot"   // отвечает Эмма (возможна пауза автопилота)
	DialogModeHuman = "human" // менеджер забрал диалог кнопкой
)

// Lead — §8.1. Строка Kanban-доски: один Telegram-пользователь = один лид.
type Lead struct {
	ID               int64          `gorm:"column:id;primaryKey"`
	TelegramUserID   int64          `gorm:"column:telegram_user_id"` // NOT NULL, UNIQUE по живым (0011); при erasure хешируется, не NULL (CLAUDE.md §4.8)
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
	DialogMode       string         `gorm:"column:dialog_mode;default:bot"` // M13: bot | human (CHECK в БД); default обязателен — без него GORM вставлял бы '' мимо DEFAULT БД
	BotSilencedUntil *time.Time     `gorm:"column:bot_silenced_until"`      // M13: пауза автопилота; NULL/прошлое = не молчит
	TakenBy          *int64         `gorm:"column:taken_by"`                // M13: id менеджера, взявшего диалог
	DeletedAt        gorm.DeletedAt `gorm:"column:deleted_at"`              // LGPD erasure = soft delete; выборки сами исключают стёртых
	CreatedAt        time.Time      `gorm:"column:created_at"`
}

func (Lead) TableName() string { return "leads" }

// BotSilenced — Эмме отвечать нельзя: диалог у менеджера (human) либо идёт
// пауза автопилота. Просроченная пауза равна её отсутствию — отдельный крон
// снятия не нужен, проверка по месту (task M13 §5).
func (l *Lead) BotSilenced(now time.Time) bool {
	if l.DialogMode == DialogModeHuman {
		return true
	}
	return l.BotSilencedUntil != nil && l.BotSilencedUntil.After(now)
}

// Message — §8.2. Одна реплика диалога (лид, бот или менеджер — M12).
type Message struct {
	ID        int64     `gorm:"column:id;primaryKey"`
	LeadID    int64     `gorm:"column:lead_id"`
	Direction string    `gorm:"column:direction"` // inbound | outbound (CHECK в БД)
	Author    *string   `gorm:"column:author"`    // 'bot' | 'manager:<id>'; NULL = bot в старых строках (0012)
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

// KnowledgeChunk — чанк базы знаний RAG (M4, §7.1). Уникальность
// (source, chunk_index): переиндексация документа заменяет чанки, не плодит.
type KnowledgeChunk struct {
	ID         int64  `gorm:"column:id;primaryKey"`
	Source     string `gorm:"column:source"`
	ChunkIndex int    `gorm:"column:chunk_index"`
	Content    string `gorm:"column:content"`
	// type в теге обязателен: без него GORM принимает слайс за has-many
	// ассоциацию и падает на разборе модели (vector(1024), voyage-3).
	Embedding Vector    `gorm:"column:embedding;type:vector(1024)"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (KnowledgeChunk) TableName() string { return "knowledge_chunks" }

// ConversationSummary — сводка диалога лида (M4, §7.3). Одна строка на лида,
// перезаписывается каждые 15 inbound; MessageCount — счётчик лида на момент
// генерации (свежесть: старая сводка не перетирает более новую).
type ConversationSummary struct {
	LeadID       int64     `gorm:"column:lead_id;primaryKey"`
	Content      string    `gorm:"column:content"`
	MessageCount int       `gorm:"column:message_count"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

func (ConversationSummary) TableName() string { return "conversation_summaries" }

// Manager — §5.2/M7. Учётка сотрудника CRM: вход по email, роль admin|manager
// (CHECK в БД). PasswordHash — только bcrypt: пароли в открытом виде не
// хранятся (критерий приёмки M7). Деактивация — active=false, не удаление.
type Manager struct {
	ID           int64     `gorm:"column:id;primaryKey"`
	Email        string    `gorm:"column:email"` // UNIQUE NOT NULL — логин
	Name         *string   `gorm:"column:name"`
	PasswordHash string    `gorm:"column:password_hash"`
	Role         string    `gorm:"column:role"` // admin | manager (CHECK в БД)
	Active       bool      `gorm:"column:active"`
	CreatedAt    time.Time `gorm:"column:created_at"`
}

func (Manager) TableName() string { return "managers" }

// RefreshToken — §5.1/M7. Серверная сторона refresh-токена: сам opaque UUID
// живёт только в HttpOnly cookie клиента, здесь — hex(SHA-256) от него.
// Ротация при /auth/refresh: строка атомарно удаляется (repo.Consume),
// взамен выдаётся новая — повтор старого токена невозможен.
type RefreshToken struct {
	ID        int64     `gorm:"column:id;primaryKey"`
	TokenHash string    `gorm:"column:token_hash"` // UNIQUE — hex(sha256(uuid))
	ManagerID int64     `gorm:"column:manager_id"`
	ExpiresAt time.Time `gorm:"column:expires_at"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (RefreshToken) TableName() string { return "refresh_tokens" }

// Setting — M13 (0014). Key/value-настройка CRM, редактируемая из UI;
// отсутствие строки = дефолт из кода (internal/settings). Значение — TEXT:
// таблица общая для будущих ключей блока 3, типизация — на слое сервиса.
type Setting struct {
	Key       string    `gorm:"column:key;primaryKey"`
	Value     string    `gorm:"column:value"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (Setting) TableName() string { return "settings" }

// All — реестр всех персистентных моделей для cmd/schema-lint.
// Добавил модель — добавь её сюда, иначе lint её не проверит.
func All() []interface{} {
	return []interface{}{
		Lead{},
		Message{},
		PaymentEvent{},
		RagAudit{},
		LGPDAudit{},
		KnowledgeChunk{},
		ConversationSummary{},
		Manager{},
		RefreshToken{},
		Setting{},
	}
}
