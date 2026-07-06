// ttl.go — TTL task management (SRS §6.4, AQ-fix #5). Контракт M3 → M5.
//
// Смена стадии Kanban (M5) ставит лиду отложенную задачу ttl:expire
// (ProcessIn), её ID хранится в leads.ttl_task_id. Reset TTL по активности
// лида = inspector.DeleteTask старой + enqueue новой (CLAUDE.md §4.7: TTL
// считается от last_activity_at, а не от created_at).
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
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// TypeTTLExpire — тип отложенной задачи «TTL лида истёк».
// Обработчик (перевод стадии) появится в M5; M3 даёт только управление.
const TypeTTLExpire = "ttl:expire"

// TypeTTLWarn — отложенная задача «TTL истекает через warnBefore» (M9):
// обработчик публикует событие ttl_warning в crm:events, стадию не меняет.
// Живёт парой с ttl:expire: взводится и снимается вместе с ней.
const TypeTTLWarn = "ttl:warn"

// ttlQueue — очередь TTL-задач (default, как у process:inbound).
const ttlQueue = "default"

// TTLPayload — полезная нагрузка ttl:expire.
type TTLPayload struct {
	LeadID int64 `json:"lead_id"`
}

// TTLWarnPayload — полезная нагрузка ttl:warn. StageID фиксирует стадию
// на момент взвода: обработчик сверяет её с фактической и молча гасит
// устаревшее предупреждение (стадия сменилась — TTL уже другой).
type TTLWarnPayload struct {
	LeadID  int64 `json:"lead_id"`
	StageID int16 `json:"stage_id"`
}

// TTLDedupKey — детерминированный TaskID TTL-задачи лида (CLAUDE.md §4.5:
// дедупликация только явным TaskID). У лида в один момент максимум одна
// TTL-задача: гонка двух Schedule не оставит «сироту», не записанную в
// leads.ttl_task_id.
//
// asynq.Unique() здесь НЕ используется сознательно: unique-замок не снимается
// при inspector.DeleteTask (проверено в тестах M3) и жил бы весь uniqueTTL —
// reset TTL по §4.7 (DeleteTask + новый enqueue) блокировался бы на час.
// TaskID даёт ту же гарантию «не больше одной задачи», но освобождается
// вместе с задачей.
func TTLDedupKey(leadID int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("ttl:%d", leadID)))
	return hex.EncodeToString(sum[:])
}

// TTLWarnDedupKey — детерминированный TaskID ttl:warn лида. ID в БД не
// хранится (как у antispam-задач M5): задача полностью управляется по
// этому ключу.
func TTLWarnDedupKey(leadID int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("ttlwarn:%d", leadID)))
	return hex.EncodeToString(sum[:])
}

// TTLManager — schedule/cancel TTL-задач лида (§6.4).
type TTLManager struct {
	client    *asynq.Client
	inspector *asynq.Inspector
	leads     repo.LeadRepo
	// warnBefore — за сколько до ttl:expire срабатывает ttl:warn
	// (kanban.ttl_warning_hours, M9). 0 = предупреждения не взводятся.
	warnBefore time.Duration
}

func NewTTLManager(cfg config.RedisConfig, leads repo.LeadRepo, warnBefore time.Duration) *TTLManager {
	opt := redisConnOpt(cfg)
	return &TTLManager{
		client:     asynq.NewClient(opt),
		inspector:  asynq.NewInspector(opt),
		leads:      leads,
		warnBefore: warnBefore,
	}
}

// Schedule (пере)ставит TTL-задачу лида: снимает старую (если была), ставит
// новую с ProcessIn(delay) и сохраняет её ID в leads.ttl_task_id (§6.4).
// Reset TTL — это просто повторный Schedule (CLAUDE.md §4.7).
func (m *TTLManager) Schedule(ctx context.Context, leadID int64, delay time.Duration) error {
	lead, err := m.leads.GetByID(ctx, leadID)
	if err != nil {
		return fmt.Errorf("queue: ttl schedule: лид %d: %w", leadID, err)
	}
	// Снимаем старую задачу и по сохранённому ID, и по детерминированному:
	// второй подчищает «сироту», если прошлый Schedule упал между enqueue
	// и записью ttl_task_id.
	if lead.TTLTaskID != nil {
		if err := m.deleteTask(*lead.TTLTaskID); err != nil {
			return fmt.Errorf("queue: ttl schedule: снятие старой задачи: %w", err)
		}
	}
	if err := m.deleteTask(TTLDedupKey(leadID)); err != nil {
		return fmt.Errorf("queue: ttl schedule: снятие задачи по dedup-ключу: %w", err)
	}
	// Старое предупреждение снимается всегда, даже если новое не взведётся
	// (warnBefore >= delay): иначе оно сработало бы по уже пересчитанному TTL.
	if err := m.deleteTask(TTLWarnDedupKey(leadID)); err != nil {
		return fmt.Errorf("queue: ttl schedule: снятие старого ttl:warn: %w", err)
	}

	payload, err := json.Marshal(TTLPayload{LeadID: leadID})
	if err != nil {
		return fmt.Errorf("queue: ttl schedule: marshal: %w", err)
	}
	info, err := m.client.EnqueueContext(ctx,
		asynq.NewTask(TypeTTLExpire, payload),
		asynq.TaskID(TTLDedupKey(leadID)), // CLAUDE.md §4.5; Unique — см. TTLDedupKey
		asynq.ProcessIn(delay),            // §6.4: отложенный запуск
		asynq.Queue(ttlQueue),
		asynq.MaxRetry(maxRetry),
	)
	if errors.Is(err, asynq.ErrTaskIDConflict) {
		// Параллельный Schedule успел поставить задачу между нашим delete
		// и enqueue — у лида ровно одна TTL-задача, цель достигнута.
		return ErrDuplicate
	}
	if err != nil {
		return fmt.Errorf("queue: ttl schedule: enqueue: %w", err)
	}

	// info.ID → leads.ttl_task_id (§6.4): по нему задача отменяется.
	if err := m.leads.UpdateFields(ctx, leadID,
		map[string]interface{}{"ttl_task_id": info.ID}); err != nil {
		// ID не сохранён — задача осталась бы неуправляемой; снимаем её.
		if derr := m.deleteTask(info.ID); derr != nil {
			return fmt.Errorf("queue: ttl schedule: сохранение id (%v) и откат задачи: %w", err, derr)
		}
		return fmt.Errorf("queue: ttl schedule: сохранение id: %w", err)
	}

	return m.scheduleWarn(ctx, lead.StageID, leadID, delay)
}

// scheduleWarn взводит ttl:warn за warnBefore до истечения TTL (M9).
// Для коротких TTL (delay <= warnBefore) предупреждение бессмысленно —
// оно сработало бы немедленно, раньше события, о котором предупреждает.
// Ошибка возвращается наверх: ретрай повторит весь Schedule идемпотентно.
func (m *TTLManager) scheduleWarn(ctx context.Context, stageID int16, leadID int64, delay time.Duration) error {
	if m.warnBefore <= 0 || delay <= m.warnBefore {
		return nil
	}
	payload, err := json.Marshal(TTLWarnPayload{LeadID: leadID, StageID: stageID})
	if err != nil {
		return fmt.Errorf("queue: ttl warn: marshal: %w", err)
	}
	_, err = m.client.EnqueueContext(ctx,
		asynq.NewTask(TypeTTLWarn, payload),
		asynq.TaskID(TTLWarnDedupKey(leadID)), // CLAUDE.md §4.5; Unique — см. TTLDedupKey
		asynq.ProcessIn(delay-m.warnBefore),
		asynq.Queue(ttlQueue),
		asynq.MaxRetry(maxRetry),
	)
	if errors.Is(err, asynq.ErrTaskIDConflict) {
		return nil // конкурентный Schedule успел взвести — цель достигнута
	}
	if err != nil {
		return fmt.Errorf("queue: ttl warn: enqueue: %w", err)
	}
	return nil
}

// Cancel снимает TTL-задачу лида (переход в терминальную стадию, ручное
// вмешательство менеджера) и чистит leads.ttl_task_id.
func (m *TTLManager) Cancel(ctx context.Context, leadID int64) error {
	lead, err := m.leads.GetByID(ctx, leadID)
	if err != nil {
		return fmt.Errorf("queue: ttl cancel: лид %d: %w", leadID, err)
	}
	// ttl:warn снимается независимо от ttl_task_id: expire-задача могла уже
	// сработать (ttl_task_id очищен ActorTTL-веткой M5), а предупреждение —
	// нет, если warn-окно ещё не наступило после re-anchor.
	if err := m.deleteTask(TTLWarnDedupKey(leadID)); err != nil {
		return fmt.Errorf("queue: ttl cancel: снятие ttl:warn: %w", err)
	}
	if lead.TTLTaskID == nil {
		return nil // нечего снимать
	}
	if err := m.deleteTask(*lead.TTLTaskID); err != nil {
		return fmt.Errorf("queue: ttl cancel: %w", err)
	}
	if err := m.leads.UpdateFields(ctx, leadID,
		map[string]interface{}{"ttl_task_id": nil}); err != nil {
		return fmt.Errorf("queue: ttl cancel: очистка ttl_task_id: %w", err)
	}
	return nil
}

// deleteTask — inspector.DeleteTask (§6.4); «задачи уже нет» — не ошибка:
// она могла успеть выполниться или быть снятой ранее.
func (m *TTLManager) deleteTask(taskID string) error {
	err := m.inspector.DeleteTask(ttlQueue, taskID)
	switch {
	case err == nil,
		errors.Is(err, asynq.ErrTaskNotFound),
		errors.Is(err, asynq.ErrQueueNotFound):
		return nil
	default:
		return fmt.Errorf("delete task %s: %w", taskID, err)
	}
}

// Close закрывает соединения client и inspector.
func (m *TTLManager) Close() error {
	cerr := m.client.Close()
	ierr := m.inspector.Close()
	if cerr != nil {
		return fmt.Errorf("queue: ttl close client: %w", cerr)
	}
	if ierr != nil {
		return fmt.Errorf("queue: ttl close inspector: %w", ierr)
	}
	return nil
}
