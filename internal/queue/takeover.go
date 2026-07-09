// takeover.go — отложенные задачи контура human takeover (M13, LOGIC-01):
//
//	takeover:reminder — «клиент ждёт reminder_minutes» → уведомление менеджеру;
//	takeover:pickup   — «менеджер так и не ответил» → Эмма подхватывает.
//
// Дедуп — CLAUDE.md §4.5: TaskID по (lead, message_count) + Unique. Повторная
// доставка того же telegram-апдейта не плодит напоминаний; новое сообщение
// клиента меняет message_count и взводит СВОЮ пару задач, а устаревшие
// гаснут в обработчике сами (проверка «менеджер ответил после инициирующего
// inbound» смотрит на факт ответа, не на конкретный message_id).
// Задачи не отменяются через inspector — только no-op по месту, поэтому
// Unique здесь безопасен (урок TTLDedupKey не применим).
package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
)

// Типы задач контура takeover (M13).
const (
	TypeTakeoverReminder = "takeover:reminder"
	TypeTakeoverPickup   = "takeover:pickup"
)

// TakeoverPayload — общая полезная нагрузка reminder/pickup: pickup ставится
// из обработчика reminder с тем же payload.
type TakeoverPayload struct {
	LeadID int64 `json:"lead_id"`
	// MessageCount — счётчик лида на момент инициирующего inbound: участвует
	// в дедуп-ключе (§4.5), в проверках обработчиков не используется.
	MessageCount int `json:"message_count"`
	// InboundMsgID — id (PK messages) инициирующего inbound: от него
	// считается «менеджер ответил после».
	InboundMsgID int64 `json:"inbound_msg_id"`
	// InboundAt — created_at инициирующего inbound: из него обработчик
	// считает waiting_minutes для уведомления.
	InboundAt time.Time `json:"inbound_at"`
	// TgMsgID — telegram message_id инициирующего inbound: pickup ставит
	// process:inbound с боевым дедуп-ключом DedupKey(lead, msg).
	TgMsgID int `json:"tg_msg_id"`
}

// TakeoverReminderKey — детерминированный TaskID takeover:reminder (§4.5).
func TakeoverReminderKey(leadID int64, messageCount int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("takeover:reminder:%d:%d", leadID, messageCount)))
	return hex.EncodeToString(sum[:])
}

// TakeoverPickupKey — детерминированный TaskID takeover:pickup (§4.5).
func TakeoverPickupKey(leadID int64, messageCount int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("takeover:pickup:%d:%d", leadID, messageCount)))
	return hex.EncodeToString(sum[:])
}

// TakeoverEnqueuer — контракт постановки задач takeover для воркера
// (в тестах — фейк). Оба метода возвращают ErrDuplicate, если задача с тем
// же (lead, message_count) уже стоит.
type TakeoverEnqueuer interface {
	EnqueueTakeoverReminder(ctx context.Context, p TakeoverPayload, delay time.Duration) error
	EnqueueTakeoverPickup(ctx context.Context, p TakeoverPayload, delay time.Duration) error
}

func (c *Client) EnqueueTakeoverReminder(ctx context.Context, p TakeoverPayload, delay time.Duration) error {
	return c.enqueueTakeover(ctx, TypeTakeoverReminder,
		TakeoverReminderKey(p.LeadID, p.MessageCount), p, delay)
}

func (c *Client) EnqueueTakeoverPickup(ctx context.Context, p TakeoverPayload, delay time.Duration) error {
	return c.enqueueTakeover(ctx, TypeTakeoverPickup,
		TakeoverPickupKey(p.LeadID, p.MessageCount), p, delay)
}

func (c *Client) enqueueTakeover(ctx context.Context, typ, taskID string, p TakeoverPayload, delay time.Duration) error {
	payload, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("queue: marshal %s: %w", typ, err)
	}
	// Unique покрывает окно до срабатывания (delay) с запасом: TaskID держит
	// уникальность, пока задача стоит, Unique — страховка §4.5 поверх.
	_, err = c.c.EnqueueContext(ctx, asynq.NewTask(typ, payload),
		asynq.TaskID(taskID),          // CLAUDE.md §4.5
		asynq.Unique(delay+uniqueTTL), // CLAUDE.md §4.5
		asynq.ProcessIn(delay),        // тайминги владельца — из settings
		asynq.MaxRetry(maxRetry),      // SRS §6.3, backoff задаёт воркер
	)
	switch {
	case errors.Is(err, asynq.ErrTaskIDConflict), errors.Is(err, asynq.ErrDuplicateTask):
		return ErrDuplicate
	case err != nil:
		return fmt.Errorf("queue: enqueue %s: %w", typ, err)
	}
	return nil
}
