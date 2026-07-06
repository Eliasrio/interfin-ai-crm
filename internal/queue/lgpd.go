// lgpd.go — периодическая задача lgpd:retention (M8, SRS §9.1):
// раз в сутки физически удалять лидов, стёртых erasure'ом > 90 дней назад.
//
// Механика: robfig/cron (та же библиотека, что внутри asynq.Scheduler) по
// расписанию зовёт EnqueueRetention, который кладёт задачу в общую очередь;
// выполняет её воркер (internal/worker/lgpd.go). asynq.Scheduler здесь
// не годится: его опции статичны, а дедуп-ключу нужна текущая дата.
//
// Дедуп между репликами приложения (у каждой свой cron) — CLAUDE.md §4.5,
// две линии:
//   - детерминированный TaskID "lgpd:retention:<дата UTC>" — реплики одного
//     дня коллапсируют в одну задачу; дата в ID обязательна: статический ID
//     конфликтовал бы с вчерашней архивной копией (dead letter живёт днями),
//     и после первого же провала cron встал бы навсегда;
//   - unique-замок 23ч (< суток: к следующему запуску гарантированно истёк) —
//     страхует окно, когда сегодняшняя задача уже выполнена и её TaskID
//     освободился, а отставшая реплика пытается поставить её повторно.
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"github.com/robfig/cron/v3"

	"github.com/interfin/interfin-ai-crm/internal/config"
)

// TypeLGPDRetention — тип задачи retention-cron (§9.1).
const TypeLGPDRetention = "lgpd:retention"

// retentionCron — 04:00 UTC ежедневно: ночное окно, вне часов менеджеров.
const retentionCron = "0 4 * * *"

// enqueueTimeout — потолок на постановку задачи из cron-колбэка
// (у колбэка нет внешнего контекста).
const enqueueTimeout = 30 * time.Second

// RetentionScheduler — суточный планировщик lgpd:retention.
type RetentionScheduler struct {
	c    *asynq.Client
	cron *cron.Cron
	log  *slog.Logger
}

// NewRetentionScheduler собирает планировщик. Часовой пояс — UTC:
// cron-время не зависит от TZ хоста.
func NewRetentionScheduler(cfg config.RedisConfig, log *slog.Logger) (*RetentionScheduler, error) {
	r := &RetentionScheduler{
		c:    asynq.NewClient(redisConnOpt(cfg)),
		cron: cron.New(cron.WithLocation(time.UTC)),
		log:  log,
	}
	if _, err := r.cron.AddFunc(retentionCron, r.tick); err != nil {
		_ = r.c.Close()
		return nil, fmt.Errorf("queue: cron %s: %w", TypeLGPDRetention, err)
	}
	return r, nil
}

// tick — cron-колбэк: ставит задачу, дубль от другой реплики — штатный исход.
func (r *RetentionScheduler) tick() {
	ctx, cancel := context.WithTimeout(context.Background(), enqueueTimeout)
	defer cancel()
	switch err := r.EnqueueRetention(ctx); {
	case errors.Is(err, ErrDuplicate):
		r.log.Info("queue: lgpd:retention уже поставлена другой репликой")
	case err != nil:
		// Не фатально: следующий запуск — завтра, ретеншен нагонит
		// (DeleteErasedBefore идемпотентен и смотрит на cutoff, не на дату).
		r.log.Error("queue: постановка lgpd:retention не удалась", "error", err)
	default:
		r.log.Info("queue: lgpd:retention поставлена")
	}
}

// EnqueueRetention ставит lgpd:retention с дедупом §4.5:
// TaskID = lgpd:retention:<дата UTC> + Unique(23ч).
func (r *RetentionScheduler) EnqueueRetention(ctx context.Context) error {
	taskID := fmt.Sprintf("%s:%s", TypeLGPDRetention, time.Now().UTC().Format("2006-01-02"))
	_, err := r.c.EnqueueContext(ctx, asynq.NewTask(TypeLGPDRetention, nil),
		asynq.TaskID(taskID),       // CLAUDE.md §4.5
		asynq.Unique(23*time.Hour), // CLAUDE.md §4.5
		asynq.MaxRetry(maxRetry),   // SRS §6.3
	)
	switch {
	case errors.Is(err, asynq.ErrTaskIDConflict), errors.Is(err, asynq.ErrDuplicateTask):
		return ErrDuplicate
	case err != nil:
		return fmt.Errorf("queue: enqueue %s: %w", TypeLGPDRetention, err)
	}
	return nil
}

// Start запускает планировщик (неблокирующе).
func (r *RetentionScheduler) Start() error {
	r.cron.Start()
	return nil
}

// Shutdown останавливает cron (дожидаясь работающего колбэка) и закрывает
// клиент очереди.
func (r *RetentionScheduler) Shutdown() {
	<-r.cron.Stop().Done()
	if err := r.c.Close(); err != nil {
		r.log.Warn("queue: retention scheduler close", "error", err)
	}
}
