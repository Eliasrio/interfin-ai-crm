// kanban.go — asynq-обработчики задач state machine (M5, SRS §3.4/§3.5).
//
// Тонкий слой: разбор payload + маршрут в kanban.Machine. Возврат ошибки =
// ретрай Asynq (3×, backoff 2/8/32с — server.go), битый payload — SkipRetry,
// как у process:inbound.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
)

// StateMachine — контракт kanban.Machine для воркера (в тестах — фейк).
type StateMachine interface {
	// OnInbound — авто-триггеры на входящее сообщение (§3.2, §3.4, §3.5).
	// silenced=true — anti-spam лимит: воркер НЕ отвечает лиду.
	OnInbound(ctx context.Context, lead *models.Lead) (silenced bool, err error)
	// HandleTTLExpire — ttl:expire: стадия истекла → Stage 8 (§3.4).
	HandleTTLExpire(ctx context.Context, leadID int64) error
	// HandleAntiSpamFollowup — antispam:followup: follow-up через 24ч (§3.5).
	HandleAntiSpamFollowup(ctx context.Context, p queue.AntiSpamPayload) error
	// HandleAntiSpamEscalate — antispam:escalate: эскалация через 48ч (§3.5).
	HandleAntiSpamEscalate(ctx context.Context, p queue.AntiSpamPayload) error
}

// KanbanHandlers — обработчики отложенных задач M5 для регистрации в Server.
type KanbanHandlers struct {
	machine StateMachine
	log     *slog.Logger
}

func NewKanbanHandlers(machine StateMachine, log *slog.Logger) *KanbanHandlers {
	return &KanbanHandlers{machine: machine, log: log}
}

// HandleTTLExpire — handler ttl:expire (тип задачи из M3, обработчик — M5).
func (h *KanbanHandlers) HandleTTLExpire(ctx context.Context, t *asynq.Task) error {
	var p queue.TTLPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("worker: payload ttl:expire не разобран: %v: %w", err, asynq.SkipRetry)
	}
	return h.machine.HandleTTLExpire(ctx, p.LeadID)
}

// HandleAntiSpamFollowup — handler antispam:followup.
func (h *KanbanHandlers) HandleAntiSpamFollowup(ctx context.Context, t *asynq.Task) error {
	p, err := antiSpamPayload(t)
	if err != nil {
		return err
	}
	return h.machine.HandleAntiSpamFollowup(ctx, p)
}

// HandleAntiSpamEscalate — handler antispam:escalate.
func (h *KanbanHandlers) HandleAntiSpamEscalate(ctx context.Context, t *asynq.Task) error {
	p, err := antiSpamPayload(t)
	if err != nil {
		return err
	}
	return h.machine.HandleAntiSpamEscalate(ctx, p)
}

func antiSpamPayload(t *asynq.Task) (queue.AntiSpamPayload, error) {
	var p queue.AntiSpamPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return p, fmt.Errorf("worker: payload %s не разобран: %v: %w", t.Type(), err, asynq.SkipRetry)
	}
	return p, nil
}
