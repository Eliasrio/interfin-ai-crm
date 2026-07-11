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
	Language         *string        `gorm:"column:language"`                // M14: ru | en | es (CHECK в БД); NULL = не определён (лид не прислал текста) — везде fallback ru
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

// --- Панель Эммы (EP-01, миграции 0016–0020, ТЗ EMMA_PANEL_TZ_v2 §5) ---

// Стили общения Эммы (CHECK в emma_prompt_versions).
const (
	EmmaStyleFormal   = "formal"
	EmmaStyleFriendly = "friendly"
	EmmaStyleNeutral  = "neutral"
	EmmaStyleExpert   = "expert"
)

// Статусы RAG-индексации файла базы знаний (CHECK в emma_kb_files).
const (
	EmmaKBPending = "pending"
	EmmaKBIndexed = "indexed"
	EmmaKBError   = "error"
)

// Типы событий журнала Эммы (CHECK в emma_events).
const (
	EmmaEventReply    = "reply"
	EmmaEventFileSent = "file_sent"
	EmmaEventHandoff  = "handoff"
	EmmaEventError    = "error"
)

// EmmaPromptVersion — версия системного промпта Эммы (0016). Активная
// версия ровно одна — частичный уникальный индекс WHERE is_current.
type EmmaPromptVersion struct {
	ID           int64  `gorm:"column:id;primaryKey"`
	SystemPrompt string `gorm:"column:system_prompt"`
	// default обязателен: пустой JSONB кодируется NULL (Value → nil) и без
	// него вставка падала бы на NOT NULL (грабля M13 dialog_mode).
	ForbiddenTopics JSONB     `gorm:"column:forbidden_topics;type:jsonb;default:'[]'"`
	Style           string    `gorm:"column:style;default:neutral"` // CHECK в БД
	IsCurrent       bool      `gorm:"column:is_current"`
	CreatedBy       *int64    `gorm:"column:created_by"` // NULL после удаления менеджера (SET NULL)
	CreatedAt       time.Time `gorm:"column:created_at"`
}

func (EmmaPromptVersion) TableName() string { return "emma_prompt_versions" }

// EmmaKBFile — файл базы знаний панели (0017). Сам файл — data/emma/kb/<uuid>,
// UNIQUE(filename): повторная загрузка = замена.
type EmmaKBFile struct {
	ID          int64     `gorm:"column:id;primaryKey"`
	Filename    string    `gorm:"column:filename"`
	MimeType    string    `gorm:"column:mime_type"`
	FilePath    string    `gorm:"column:file_path"`
	FileSize    int64     `gorm:"column:file_size"`
	IndexStatus string    `gorm:"column:index_status;default:pending"` // CHECK в БД
	IndexError  *string   `gorm:"column:index_error"`
	ChunksCount int       `gorm:"column:chunks_count"`
	CreatedAt   time.Time `gorm:"column:created_at"`
}

func (EmmaKBFile) TableName() string { return "emma_kb_files" }

// EmmaKBSource — source чанков файла панели в knowledge_chunks:
// "panel:<file_id>". Соглашение — контракт навсегда (EP-03): смена формата
// потеряла бы связь файл↔чанки. С source файлов docs/kb (имена файлов,
// cmd/index-kb) пространства не пересекаются.
func EmmaKBSource(fileID int64) string {
	return "panel:" + strconv.FormatInt(fileID, 10)
}

// EmmaSendFile — файл, который Эмма отправляет клиентам (0018).
// IsActive задавать явно при Create: zero value затёр бы DEFAULT TRUE
// схемы (грабля M5, как Manager.Active).
type EmmaSendFile struct {
	ID          int64     `gorm:"column:id;primaryKey"`
	Name        string    `gorm:"column:name"`
	Description string    `gorm:"column:description"` // подсказка Эмме
	FilePath    string    `gorm:"column:file_path"`
	MimeType    string    `gorm:"column:mime_type"`
	FileSize    int64     `gorm:"column:file_size"`
	IsActive    bool      `gorm:"column:is_active"`
	CreatedAt   time.Time `gorm:"column:created_at"`
}

func (EmmaSendFile) TableName() string { return "emma_send_files" }

// EmmaContact — контакт/ссылка справочника Эммы (0019). IsActive — явно
// при Create (та же грабля M5).
type EmmaContact struct {
	ID        int64   `gorm:"column:id;primaryKey"`
	Type      string  `gorm:"column:type"` // CHECK в БД: phone|whatsapp|telegram|email|website|other
	Name      string  `gorm:"column:name"`
	Value     string  `gorm:"column:value"`
	Comment   *string `gorm:"column:comment"`
	IsActive  bool    `gorm:"column:is_active"`
	SortOrder int     `gorm:"column:sort_order"`
}

func (EmmaContact) TableName() string { return "emma_contacts" }

// EmmaEvent — запись журнала Эммы (0020): ответ, отправка файла, handoff,
// ошибка. lead_id/send_file_id — SET NULL при удалении родителя (LGPD ТЗ §5).
type EmmaEvent struct {
	ID             int64     `gorm:"column:id;primaryKey"`
	EventType      string    `gorm:"column:event_type"` // CHECK в БД
	ErrorKind      *string   `gorm:"column:error_kind"` // llm_api/telegram_api/timeout/file_not_found/kb_index
	Detail         *string   `gorm:"column:detail"`
	LeadID         *int64    `gorm:"column:lead_id"`
	SendFileID     *int64    `gorm:"column:send_file_id"`
	ResponseTimeMs *int      `gorm:"column:response_time_ms"`
	TokensIn       *int      `gorm:"column:tokens_in"`  // usage.input_tokens (event_type='reply')
	TokensOut      *int      `gorm:"column:tokens_out"` // usage.output_tokens
	CreatedAt      time.Time `gorm:"column:created_at"`
}

func (EmmaEvent) TableName() string { return "emma_events" }

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
		EmmaPromptVersion{},
		EmmaKBFile{},
		EmmaSendFile{},
		EmmaContact{},
		EmmaEvent{},
	}
}
