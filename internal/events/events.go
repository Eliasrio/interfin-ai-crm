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
	TypeDialogMode        = "dialog_mode"        // M13: смена режима диалога (bot/human/пауза автопилота)
	TypeTakeoverReminder  = "takeover_reminder"  // M13: клиент ждёт ответа менеджера reminder_minutes
	TypeLeadLanguage      = "lead_language"      // M14: язык лида определён (ingestion) или сменён менеджером
	TypeEmmaKBStatus      = "emma_kb_status"     // EP-03: финал индексации файла базы знаний панели
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

	// Только для dialog_mode (M13): текущее состояние режима целиком —
	// клиент не собирает его из дельт. Reason="takeover_pickup" помечает
	// подхват Эммы (фронт показывает тост).
	Mode          string     `json:"mode,omitempty"`           // bot | human
	SilencedUntil *time.Time `json:"silenced_until,omitempty"` // пауза автопилота (mode=bot)
	TakenBy       *int64     `json:"taken_by,omitempty"`       // менеджер, взявший диалог (mode=human)

	// Только для takeover_reminder (M13).
	WaitingMinutes int `json:"waiting_minutes,omitempty"` // сколько минут клиент ждёт ответа

	// Только для lead_language (M14): текущий язык лида целиком (ru|en|es),
	// как dialog_mode — клиент не собирает состояние из дельт.
	Language string `json:"language,omitempty"`

	// Только для emma_kb_status (EP-03): итог индексации файла базы знаний.
	// LeadID/StageID нулевые — событие не про лида; фронт матчит по type.
	FileID   int64  `json:"file_id,omitempty"`
	Filename string `json:"filename,omitempty"`
	KBStatus string `json:"status,omitempty"` // indexed | error
	Chunks   int    `json:"chunks,omitempty"`
	KBError  string `json:"error,omitempty"`

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

// ReasonTakeoverPickup — Reason события dialog_mode при автоподхвате Эммой
// (M13): фронт отличает его от ручного «Вернуть Эмме» и показывает тост.
const ReasonTakeoverPickup = "takeover_pickup"

// ReasonClientHandoff — Reason события dialog_mode при переводе на менеджера
// по просьбе клиента (EP-05: кнопка или маркер {{handoff}}). Существующий
// обработчик dialog_mode фронта (M13) его понимает как доп. поле; EP-07
// подсвечивает карточку «требует ответа».
const ReasonClientHandoff = "client_handoff"

// DialogModeEvent собирает событие dialog_mode из актуального лида (M13) —
// единая точка для всех публикаторов (ручка режима, автопилот, подхват).
// Событие несёт состояние целиком: mode + silenced_until + taken_by.
func DialogModeEvent(l *models.Lead, reason string) Event {
	return Event{
		Type:          TypeDialogMode,
		LeadID:        l.ID,
		StageID:       l.StageID,
		Mode:          l.DialogMode,
		SilencedUntil: l.BotSilencedUntil,
		TakenBy:       l.TakenBy,
		Reason:        reason,
	}
}

// LeadLanguageEvent собирает событие lead_language (M14) — единая точка для
// обоих публикаторов (автодетекция в ingestion, ручная смена PATCH-ручкой).
// По образцу dialog_mode: событие несёт состояние целиком.
func LeadLanguageEvent(l *models.Lead, reason string) Event {
	language := ""
	if l.Language != nil {
		language = *l.Language
	}
	return Event{
		Type:     TypeLeadLanguage,
		LeadID:   l.ID,
		StageID:  l.StageID,
		Language: language,
		Reason:   reason,
	}
}

// EmmaKBStatusEvent — финал индексации файла базы знаний (EP-03): фронт
// EP-07 обновляет строку таблицы без поллинга. Публикуется ТОЛЬКО на финале
// (indexed/error) — промежуточный pending фронт видит из ответа upload'а.
// Старые клиенты игнорируют незнакомый тип (проверено M12).
func EmmaKBStatusEvent(fileID int64, filename, status string, chunks int, indexErr *string) Event {
	e := Event{
		Type:     TypeEmmaKBStatus,
		FileID:   fileID,
		Filename: filename,
		KBStatus: status,
		Chunks:   chunks,
	}
	if indexErr != nil {
		e.KBError = *indexErr
	}
	return e
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
