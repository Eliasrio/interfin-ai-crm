// summary.go — задача фоновой генерации сводки диалога (M4, SRS §7.3).
//
// Ставится воркером process:inbound на каждом 15-м inbound-сообщении
// (kanban.summary_every_n_messages). Правило §4.5 то же, что у inbound:
// TaskID + Unique. Дедуп-ключ включает message_count: ретрай process:inbound
// того же сообщения не плодит вторую задачу, а СЛЕДУЮЩЕЕ 15-е сообщение
// (count=30) даёт новый ключ и новую задачу. Гонку конкурентных генераторов
// гасит не очередь, а Redis-замок в обработчике (IQ-5).
package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hibiken/asynq"
)

// TypeSummaryGenerate — тип задачи «сгенерировать сводку диалога лида».
const TypeSummaryGenerate = "summary:generate"

// SummaryPayload — полезная нагрузка summary:generate.
// MessageCount — счётчик лида в момент постановки: обработчик пишет его в
// conversation_summaries, а upsert по нему отбрасывает устаревшие сводки.
type SummaryPayload struct {
	LeadID       int64 `json:"lead_id"`
	MessageCount int   `json:"message_count"`
}

// SummaryDedupKey — sha256("summary:lead_id:message_count") hex (§4.5).
func SummaryDedupKey(leadID int64, messageCount int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("summary:%d:%d", leadID, messageCount)))
	return hex.EncodeToString(sum[:])
}

// SummaryEnqueuer — контракт постановки summary-задач для воркера
// (отдельный от Enqueuer M2: телеграм-хендлеру он не нужен).
type SummaryEnqueuer interface {
	// EnqueueSummary ставит summary:generate. ErrDuplicate — задача для этого
	// (leadID, messageCount) уже в очереди; для вызывающего это не сбой.
	EnqueueSummary(ctx context.Context, leadID int64, messageCount int) error
}

func (c *Client) EnqueueSummary(ctx context.Context, leadID int64, messageCount int) error {
	payload, err := json.Marshal(SummaryPayload{LeadID: leadID, MessageCount: messageCount})
	if err != nil {
		return fmt.Errorf("queue: marshal summary payload: %w", err)
	}

	task := asynq.NewTask(TypeSummaryGenerate, payload)
	_, err = c.c.EnqueueContext(ctx, task,
		asynq.TaskID(SummaryDedupKey(leadID, messageCount)), // CLAUDE.md §4.5
		asynq.Unique(uniqueTTL),                             // CLAUDE.md §4.5
		asynq.MaxRetry(maxRetry),
	)
	switch {
	case errors.Is(err, asynq.ErrTaskIDConflict), errors.Is(err, asynq.ErrDuplicateTask):
		return ErrDuplicate
	case err != nil:
		return fmt.Errorf("queue: enqueue %s: %w", TypeSummaryGenerate, err)
	}
	return nil
}
