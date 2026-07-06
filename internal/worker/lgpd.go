// lgpd.go — обработчик lgpd:retention (M8, SRS §9.1): физическое удаление
// лидов, стёртых erasure'ом больше retention_days (90) дней назад.
// Постановку задачи по cron делает queue.RetentionScheduler.
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// LGPDHandlers — обработчики LGPD-задач для Server.RegisterLGPD.
type LGPDHandlers struct {
	lgpd          repo.LGPDRepo
	retentionDays int
	log           *slog.Logger
}

func NewLGPDHandlers(lgpdRepo repo.LGPDRepo, retentionDays int, log *slog.Logger) *LGPDHandlers {
	return &LGPDHandlers{lgpd: lgpdRepo, retentionDays: retentionDays, log: log}
}

// HandleLGPDRetention — тело задачи lgpd:retention. Идемпотентна: повторный
// запуск за те же сутки удалит 0 строк. messages уходят каскадом FK,
// payment_events переживают удаление с lead_id=NULL (0010, §9.3).
func (h *LGPDHandlers) HandleLGPDRetention(ctx context.Context, _ *asynq.Task) error {
	cutoff := time.Now().UTC().AddDate(0, 0, -h.retentionDays)
	deleted, err := h.lgpd.DeleteErasedBefore(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("worker: lgpd retention: %w", err)
	}
	h.log.Info("worker: lgpd retention выполнен",
		"deleted_leads", deleted, "cutoff", cutoff, "retention_days", h.retentionDays)
	return nil
}
