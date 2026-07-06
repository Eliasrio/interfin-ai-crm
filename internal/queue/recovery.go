// recovery.go — восстановление очереди после недоступности Redis (M11, §11.2).
//
// Вторая половина graceful degradation. Первая (M2, handlers/telegram.go):
// Redis down → вебхук сохраняет inbound в Postgres, ставит leads.pending_task
// = TRUE и отвечает Telegram 200. Здесь — recovery-cron: раз в 30 секунд
// выбирает помеченных лидов и, как только Redis ожил, перевыставляет
// process:inbound и снимает флаг. Сообщения не теряются: они уже в БД,
// воркер (M3) строит контекст из истории messages, а не из payload задачи.
package queue

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

// RecoveryInterval — период опроса pending_task (§11.2: 30 секунд).
const RecoveryInterval = 30 * time.Second

// recoveryBatch — лидов за один тик. Ограничение — защита от лавины enqueue
// после долгого простоя Redis; хвост дойдёт за следующие тики.
const recoveryBatch = 100

// PendingLeadRepo — срез LeadRepo, нужный recovery-cron (в тестах — фейк).
type PendingLeadRepo interface {
	ListPendingTask(ctx context.Context, limit int) ([]models.Lead, error)
	ClearPendingTask(ctx context.Context, id int64, seenMessageCount int) (bool, error)
}

// Recovery — cron §11.2: pending_task=TRUE → enqueue → pending_task=FALSE.
type Recovery struct {
	leads    PendingLeadRepo
	enq      Enqueuer
	interval time.Duration
	log      *slog.Logger
}

// NewRecovery собирает recovery-cron. interval <= 0 → RecoveryInterval
// (нестандартный интервал нужен только тестам).
func NewRecovery(leads PendingLeadRepo, enq Enqueuer, interval time.Duration, log *slog.Logger) *Recovery {
	if interval <= 0 {
		interval = RecoveryInterval
	}
	return &Recovery{leads: leads, enq: enq, interval: interval, log: log}
}

// Run блокируется до отмены ctx (запускать горутиной из cmd/server).
// Ошибки тиков не роняют цикл: Redis down — штатное состояние для этого
// крона, он и существует ради ожидания восстановления.
func (r *Recovery) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Sweep(ctx)
		}
	}
}

// Sweep — один проход: перевыставить задачи всех помеченных лидов.
// Экспортирован для интеграционного теста M11 (тик без ожидания таймера).
func (r *Recovery) Sweep(ctx context.Context) {
	leads, err := r.leads.ListPendingTask(ctx, recoveryBatch)
	if err != nil {
		r.log.Error("recovery: выборка pending_task не удалась", "error", err)
		return
	}
	if len(leads) == 0 {
		return
	}
	r.log.Info("recovery: найдены лиды с pending_task", "count", len(leads))

	for _, lead := range leads {
		// Дедуп-ключ (CLAUDE.md §4.5) строится от -message_count: настоящих
		// telegram message_id у нас здесь нет (вебхук их не сохраняет), а
		// отрицательное значение не пересечётся с положительными msg.ID
		// вебхука. Новое сообщение = новый count = новый ключ, повторный
		// Sweep того же состояния = ErrDuplicate (не вторая задача).
		err := r.enq.EnqueueInbound(ctx, lead.ID, -lead.MessageCount)
		switch {
		case errors.Is(err, ErrDuplicate):
			// Задача уже в очереди (прошлый Sweep успел поставить, но упал
			// на снятии флага) — двигаемся к ClearPendingTask.
		case err != nil:
			// Redis всё ещё лежит — тик прекращаем целиком, ждём следующего.
			r.log.Warn("recovery: Redis недоступен, ждём следующий тик",
				"lead_id", lead.ID, "error", err)
			return
		}

		// Снятие флага условное (guard по message_count): если за время
		// enqueue пришло новое сообщение и его enqueue снова упал, флаг
		// обязан пережить этот Sweep — иначе новое сообщение потеряно
		// до следующего inbound.
		cleared, err := r.leads.ClearPendingTask(ctx, lead.ID, lead.MessageCount)
		if err != nil {
			r.log.Error("recovery: pending_task не снят", "lead_id", lead.ID, "error", err)
			continue
		}
		if !cleared {
			r.log.Info("recovery: у лида новое сообщение, флаг оставлен до следующего тика",
				"lead_id", lead.ID)
			continue
		}
		r.log.Info("recovery: задача перевыставлена, pending_task снят",
			"lead_id", lead.ID, "message_count", lead.MessageCount)
	}
}
