// antispam.go — отложенные задачи anti-spam (SRS §3.5, AQ²-fix #8). M5.
//
// При достижении лимита 25 inbound/стадию бот замолкает, и лиду ставятся
// ДВЕ отложенные задачи: antispam:followup (24ч, одно follow-up-сообщение)
// и antispam:escalate (48ч, manager_escalation + leads.escalated_at) —
// молчание не вечное, у лида всегда есть автоматический выход.
// Переход в следующую стадию снимает обе (Cancel) и сбрасывает счётчик.
//
// TaskID детерминированные, asynq.Unique НЕ используется — причина та же,
// что у TTL (см. TTLDedupKey): unique-замок не снимается DeleteTask и
// заблокировал бы перевзвод после отмены. ID в БД не храним: ключ
// восстановим из leadID, отдельная колонка не нужна (single source of
// truth схемы — CLAUDE.md §4.1 — не трогаем).
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

	"github.com/interfin/interfin-ai-crm/internal/config"
)

// Типы отложенных anti-spam задач (§3.5). Обработчики — в internal/worker.
const (
	TypeAntiSpamFollowup = "antispam:followup"
	TypeAntiSpamEscalate = "antispam:escalate"
)

// antiSpamQueue — очередь anti-spam задач (default, как у остальных).
const antiSpamQueue = "default"

// AntiSpamPayload — полезная нагрузка обеих задач. StageID — стадия лида в
// момент срабатывания лимита: обработчик сравнивает её с текущей и молча
// выходит, если лид уже перешёл дальше (страховка поверх Cancel).
type AntiSpamPayload struct {
	LeadID  int64 `json:"lead_id"`
	StageID int16 `json:"stage_id"`
}

// AntiSpamDedupKey — детерминированный TaskID (CLAUDE.md §4.5): максимум
// одна followup- и одна escalate-задача на лида. Стадия в ключ не входит:
// Cancel при переходе должен находить задачу, зная только leadID.
func AntiSpamDedupKey(taskType string, leadID int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", taskType, leadID)))
	return hex.EncodeToString(sum[:])
}

// AntiSpamScheduler — контракт для state machine (в тестах — фейк).
type AntiSpamScheduler interface {
	// Schedule взводит followup (followupIn) и escalate (escalateIn).
	// fresh=false — обе задачи уже стояли (повторный вызов после ретрая
	// или конкурентного inbound): alert публиковать не надо.
	Schedule(ctx context.Context, leadID int64, stageID int16, followupIn, escalateIn time.Duration) (fresh bool, err error)
	// Cancel снимает обе задачи лида; отсутствие задач — не ошибка.
	Cancel(ctx context.Context, leadID int64) error
}

// AntiSpamManager — боевой AntiSpamScheduler поверх Asynq.
type AntiSpamManager struct {
	client    *asynq.Client
	inspector *asynq.Inspector
}

func NewAntiSpamManager(cfg config.RedisConfig) *AntiSpamManager {
	opt := redisConnOpt(cfg)
	return &AntiSpamManager{
		client:    asynq.NewClient(opt),
		inspector: asynq.NewInspector(opt),
	}
}

func (m *AntiSpamManager) Schedule(ctx context.Context, leadID int64, stageID int16, followupIn, escalateIn time.Duration) (bool, error) {
	fresh, err := m.enqueue(ctx, TypeAntiSpamFollowup, leadID, stageID, followupIn)
	if err != nil {
		return false, err
	}
	// Отдельный enqueue: упал escalate — ретрай вызывающего довзведёт его,
	// followup при этом ответит ErrTaskIDConflict (fresh=false) и не задвоится.
	if _, err := m.enqueue(ctx, TypeAntiSpamEscalate, leadID, stageID, escalateIn); err != nil {
		return false, err
	}
	return fresh, nil
}

func (m *AntiSpamManager) enqueue(ctx context.Context, taskType string, leadID int64, stageID int16, delay time.Duration) (bool, error) {
	payload, err := json.Marshal(AntiSpamPayload{LeadID: leadID, StageID: stageID})
	if err != nil {
		return false, fmt.Errorf("queue: marshal %s: %w", taskType, err)
	}
	_, err = m.client.EnqueueContext(ctx,
		asynq.NewTask(taskType, payload),
		asynq.TaskID(AntiSpamDedupKey(taskType, leadID)), // CLAUDE.md §4.5; Unique — см. шапку файла
		asynq.ProcessIn(delay),
		asynq.Queue(antiSpamQueue),
		asynq.MaxRetry(maxRetry),
	)
	if errors.Is(err, asynq.ErrTaskIDConflict) {
		return false, nil // уже взведена — цель достигнута
	}
	if err != nil {
		return false, fmt.Errorf("queue: enqueue %s: %w", taskType, err)
	}
	return true, nil
}

func (m *AntiSpamManager) Cancel(ctx context.Context, leadID int64) error {
	for _, taskType := range []string{TypeAntiSpamFollowup, TypeAntiSpamEscalate} {
		err := m.inspector.DeleteTask(antiSpamQueue, AntiSpamDedupKey(taskType, leadID))
		switch {
		case err == nil,
			errors.Is(err, asynq.ErrTaskNotFound),
			errors.Is(err, asynq.ErrQueueNotFound):
			// нет задачи — нечего снимать (уже выполнилась или не взводилась)
		default:
			return fmt.Errorf("queue: antispam cancel %s: %w", taskType, err)
		}
	}
	return nil
}

// Close закрывает соединения client и inspector.
func (m *AntiSpamManager) Close() error {
	cerr := m.client.Close()
	ierr := m.inspector.Close()
	if cerr != nil {
		return fmt.Errorf("queue: antispam close client: %w", cerr)
	}
	if ierr != nil {
		return fmt.Errorf("queue: antispam close inspector: %w", ierr)
	}
	return nil
}
