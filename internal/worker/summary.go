// summary.go — обработчик summary:generate (M4, SRS §7.3): фоновая генерация
// сводки диалога каждые 15 inbound-сообщений.
//
// Защита от гонки (IQ-5) двухслойная:
//  1. distributed lock в Redis (§7.3): SET summary:{lead_id} 1 EX 60 NX;
//     не взял замок → nil (skip), генерит ровно один конкурент;
//  2. после взятия замка — проверка свежести по conversation_summaries:
//     сводка с message_count ≥ нашего уже записана (конкурент успел до нас,
//     пока мы ждали) → skip без вызова Claude.
//
// Плюс страховка в БД: Upsert отбрасывает сводки со старым message_count.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/interfin/interfin-ai-crm/internal/claude"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// summaryLockTTL — EX 60 из §7.3: генерация (один вызов Claude, 3–15 с)
// заведомо укладывается; упавший процесс отпускает замок сам по TTL.
const summaryLockTTL = 60 * time.Second

// summaryInputBudgetTokens — потолок входного диалога для генерации сводки.
// Держит запрос в духе IQ-6 (≤ 9000 токенов вместе с system и ответом 1000).
const summaryInputBudgetTokens = 6000

// summarySystemPrompt — роль сумматора. Размер ответа ограничивает
// max_tokens = claude_reply_tokens (1000) — ровно бюджет summary §7.2.
const summarySystemPrompt = `Ты сжимаешь диалог Telegram-бота компании INTERFIN GROUP с клиентом в краткую сводку.
Составь сводку на русском языке одним связным текстом без приветствий и преамбул.
Обязательно сохрани: потребность клиента, бюджет и срочность (если звучали),
ключевые договорённости и вопросы, оставшиеся без ответа.
Не выдумывай факты, которых в диалоге нет.`

// Locker — распределённый замок §7.3 (в проде Redis, в тестах он же
// с REDIS_TEST_ADDR или фейк).
type Locker interface {
	// TryLock — true, если замок взят (SET key 1 EX ttl NX).
	TryLock(ctx context.Context, key string, ttl time.Duration) (bool, error)
	// Unlock снимает замок (DEL key).
	Unlock(ctx context.Context, key string) error
}

// redisLocker — Locker поверх go-redis (одиночный Redis или Sentinel).
type redisLocker struct{ rdb redis.UniversalClient }

func NewRedisLocker(rdb redis.UniversalClient) Locker { return &redisLocker{rdb: rdb} }

func (l *redisLocker) TryLock(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	ok, err := l.rdb.SetNX(ctx, key, 1, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("locker: setnx %s: %w", key, err)
	}
	return ok, nil
}

func (l *redisLocker) Unlock(ctx context.Context, key string) error {
	if err := l.rdb.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("locker: del %s: %w", key, err)
	}
	return nil
}

// summaryLockKey — ключ замка §7.3: summary:{lead_id}.
func summaryLockKey(leadID int64) string { return fmt.Sprintf("summary:%d", leadID) }

// Summarizer — обработчик summary:generate.
type Summarizer struct {
	leads     repo.LeadRepo
	msgs      repo.MessageRepo
	summaries repo.SummaryRepo
	ai        Completer
	locker    Locker
	log       *slog.Logger
}

func NewSummarizer(
	leads repo.LeadRepo,
	msgs repo.MessageRepo,
	summaries repo.SummaryRepo,
	ai Completer,
	locker Locker,
	log *slog.Logger,
) *Summarizer {
	return &Summarizer{leads: leads, msgs: msgs, summaries: summaries, ai: ai, locker: locker, log: log}
}

// HandleSummaryGenerate — handler задачи summary:generate.
// nil при «замок занят» — это штатный skip §7.3, не повод для ретрая:
// сводку в этот момент пишет конкурент.
func (s *Summarizer) HandleSummaryGenerate(ctx context.Context, t *asynq.Task) error {
	var payload queue.SummaryPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("worker: payload summary:generate не разобран: %v: %w", err, asynq.SkipRetry)
	}
	log := s.log.With("lead_id", payload.LeadID, "message_count", payload.MessageCount)

	// §7.3: SET summary:{lead_id} 1 EX 60 NX → nil = занято, skip.
	lockKey := summaryLockKey(payload.LeadID)
	ok, err := s.locker.TryLock(ctx, lockKey, summaryLockTTL)
	if err != nil {
		return fmt.Errorf("worker: summary lock: %w", err)
	}
	if !ok {
		log.Info("worker: summary уже генерирует конкурент, skip (§7.3)")
		return nil
	}
	// DEL после записи (§7.3); при ошибке генерации тоже снимаем — пусть
	// ретрай не ждёт истечения TTL. Ошибка DEL не смертельна: замок умрёт по EX 60.
	defer func() {
		if err := s.locker.Unlock(context.WithoutCancel(ctx), lockKey); err != nil {
			log.Warn("worker: замок summary не снят, истечёт по TTL", "error", err)
		}
	}()

	// Свежесть: конкурент мог записать сводку, пока задача ждала в очереди.
	existing, err := s.summaries.GetByLead(ctx, payload.LeadID)
	if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return fmt.Errorf("worker: чтение текущей сводки: %w", err)
	}
	if existing != nil && existing.MessageCount >= payload.MessageCount {
		log.Info("worker: сводка уже актуальна, skip", "existing_count", existing.MessageCount)
		return nil
	}

	if _, err := s.leads.GetByID(ctx, payload.LeadID); errors.Is(err, repo.ErrNotFound) {
		log.Warn("worker: лид стёрт, сводка не нужна")
		return nil
	} else if err != nil {
		return fmt.Errorf("worker: загрузка лида: %w", err)
	}

	history, err := s.msgs.ListByLead(ctx, payload.LeadID, historyFetchLimit)
	if err != nil {
		return fmt.Errorf("worker: история для сводки: %w", err)
	}
	if len(history) == 0 {
		log.Warn("worker: история пуста, сводка не нужна")
		return nil
	}

	dialog := flattenDialog(history, existing)
	summary, err := s.ai.Complete(ctx, summarySystemPrompt, []claude.Message{
		{Role: claude.RoleUser, Content: dialog},
	})
	if err != nil {
		return fmt.Errorf("worker: генерация сводки: %w", err)
	}

	err = s.summaries.Upsert(ctx, &models.ConversationSummary{
		LeadID:       payload.LeadID,
		Content:      summary,
		MessageCount: payload.MessageCount,
	})
	if err != nil {
		return fmt.Errorf("worker: сохранение сводки: %w", err)
	}

	log.Info("worker: сводка записана", "summary_tokens_estimate", estimateTokens(summary))
	return nil
}

// flattenDialog собирает историю в один user-текст для сумматора.
// Прошлая сводка идёт первой (непрерывность: history ограничена
// historyFetchLimit, старые реплики уже только в сводке). Общий вход режется
// до summaryInputBudgetTokens с хвоста — свежее важнее.
func flattenDialog(history []models.Message, existing *models.ConversationSummary) string {
	var b strings.Builder
	if existing != nil {
		b.WriteString("Сводка более раннего диалога:\n")
		b.WriteString(existing.Content)
		b.WriteString("\n\nПродолжение диалога:\n")
	} else {
		b.WriteString("Диалог:\n")
	}
	for _, m := range history {
		text := strings.TrimSpace(m.Content)
		if text == "" {
			continue
		}
		role := "Клиент"
		if m.Direction == models.DirectionOutbound {
			role = "Бот"
		}
		b.WriteString(role)
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteString("\n")
	}
	return tailRunes(b.String(), summaryInputBudgetTokens*charsPerToken)
}
