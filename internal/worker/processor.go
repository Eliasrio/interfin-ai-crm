// processor.go — обработчик process:inbound: Worker Pipeline SRS §6.2.
//
//  1. Typing  2) Контекст (token budget)  3) Anthropic /v1/messages
//  4. Save outbound  5) bot.Send  (6: PUBLISH crm:events — появится в M5)
//
// Идемпотентность (§6.3): дубли задач гасит TaskID+Unique ещё на enqueue (M2);
// здесь — идемпотентность РЕТРАЯ. Порядок «save → send» означает, что при
// падении Send ответ уже в БД: повторная попытка видит последним outbound
// и только переотправляет его, НЕ вызывая Claude и не плодя строк в messages.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/claude"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// historyFetchLimit — сколько последних сообщений поднимать из БД.
// Дальше окно всё равно режет токен-бюджетер (history ≤ 4000 токенов §7.2).
const historyFetchLimit = 50

// Sender — отправка в Telegram (в проде обёртка над telebot, в тестах фейк).
type Sender interface {
	// Typing показывает лиду «печатает…» (§6.2 шаг 1). Best effort.
	Typing(chatID int64) error
	// Send отправляет текст в чат.
	Send(chatID int64, text string) error
}

// Completer — вызов Claude (в проде *claude.Client, в тестах фейк).
type Completer interface {
	Complete(ctx context.Context, system string, msgs []claude.Message) (string, error)
}

// Processor — зависимости обработчика process:inbound.
type Processor struct {
	leads    repo.LeadRepo
	msgs     repo.MessageRepo
	budgeter *Budgeter
	ai       Completer
	sender   Sender
	log      *slog.Logger
}

func NewProcessor(
	leads repo.LeadRepo,
	msgs repo.MessageRepo,
	budgeter *Budgeter,
	ai Completer,
	sender Sender,
	log *slog.Logger,
) *Processor {
	return &Processor{leads: leads, msgs: msgs, budgeter: budgeter, ai: ai, sender: sender, log: log}
}

// HandleProcessInbound — handler задачи process:inbound (контракт M2→M3).
// Возврат ошибки = ретрай Asynq (3×, backoff 2/8/32с — server.go), затем
// dead letter + алерт (§6.3). Возврат nil = задача выполнена.
func (p *Processor) HandleProcessInbound(ctx context.Context, t *asynq.Task) error {
	var payload queue.InboundPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		// Битый payload не починится ретраем — сразу в архив (§6.3).
		return fmt.Errorf("worker: payload process:inbound не разобран: %v: %w", err, asynq.SkipRetry)
	}
	log := p.log.With("lead_id", payload.LeadID, "tg_msg_id", payload.MsgID)

	lead, err := p.leads.GetByID(ctx, payload.LeadID)
	if errors.Is(err, repo.ErrNotFound) {
		// Лид стёрт (LGPD erasure) между enqueue и обработкой — задача неактуальна.
		log.Warn("worker: лид не найден, задача пропущена")
		return nil
	}
	if err != nil {
		return fmt.Errorf("worker: загрузка лида: %w", err)
	}

	history, err := p.msgs.ListByLead(ctx, lead.ID, historyFetchLimit)
	if err != nil {
		return fmt.Errorf("worker: история сообщений: %w", err)
	}
	if len(history) == 0 {
		log.Warn("worker: у лида нет сообщений, отвечать не на что")
		return nil
	}

	// Идемпотентность ретрая: последний в истории outbound = ответ уже
	// сгенерирован и сохранён (шаг 4 прошёл), упасть мог только Send.
	// Переотправляем сохранённое, к Claude не ходим.
	if last := history[len(history)-1]; last.Direction == models.DirectionOutbound {
		log.Info("worker: ответ уже сохранён, переотправка без вызова Claude")
		if err := p.sender.Send(lead.TelegramUserID, last.Content); err != nil {
			return fmt.Errorf("worker: переотправка ответа: %w", err)
		}
		return nil
	}

	// 1) Typing — best effort: недоставленный индикатор не стоит ретрая.
	if err := p.sender.Typing(lead.TelegramUserID); err != nil {
		log.Warn("worker: typing не отправлен", "error", err)
	}

	// 2) Контекст в пределах бюджета §7.2. Summary пустой до M4 (§7.3).
	system, msgs, stats, err := p.budgeter.Build(ctx, systemPrompt, "", history)
	if err != nil {
		return fmt.Errorf("worker: сборка контекста: %w", err)
	}

	// 3) Claude. Ошибка (5xx, rate limit, сеть) → ретрай, затем dead letter.
	reply, err := p.ai.Complete(ctx, system, msgs)
	if err != nil {
		return fmt.Errorf("worker: вызов claude: %w", err)
	}

	// 4) Save outbound. Счётчики лида НЕ трогаем (CLAUDE.md §4.3).
	replyTokens := estimateTokens(reply)
	out := &models.Message{LeadID: lead.ID, Content: reply, Tokens: &replyTokens}
	if err := p.msgs.CreateOutbound(ctx, out); err != nil {
		return fmt.Errorf("worker: сохранение ответа: %w", err)
	}

	// 5) Send. При падении ретрай уйдёт в ветку переотправки выше.
	if err := p.sender.Send(lead.TelegramUserID, reply); err != nil {
		return fmt.Errorf("worker: отправка ответа: %w", err)
	}

	log.Info("worker: ответ отправлен",
		"estimate_tokens", stats.EstimateTokens,
		"exact_tokens", stats.ExactTokens,
		"count_tokens_calls", stats.ExactCalls,
		"dropped_history", stats.DroppedHistory,
		"reply_tokens_estimate", replyTokens,
	)
	return nil
}
