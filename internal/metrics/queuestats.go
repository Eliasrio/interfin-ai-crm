// queuestats.go — опрос очередей Asynq для asynq_queue_size (SRS §14).
//
// Гистограммы пишутся в момент события, а размер очереди — состояние, его
// надо опрашивать: Inspector раз в interval снимает статистику всех очередей.
// Ключевое состояние — dead (архив asynq, §6.3): его рост = задачи, исчерпавшие
// ретраи; алерт DeadLetterQueueGrowing (ops/prometheus/alerts.yml).
package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
)

// QueueStatsInterval — период опроса. 15 с достаточно: алерты §14 смотрят
// на тренды минутного масштаба.
const QueueStatsInterval = 15 * time.Second

// QueueStats — поллер asynq_queue_size.
type QueueStats struct {
	inspector *asynq.Inspector
	interval  time.Duration
	log       *slog.Logger
}

// NewQueueStats — поллер поверх того же Redis-подключения, что у очереди
// (queue.ConnOpt: single или Sentinel). interval <= 0 → QueueStatsInterval.
func NewQueueStats(opt asynq.RedisConnOpt, interval time.Duration, log *slog.Logger) *QueueStats {
	if interval <= 0 {
		interval = QueueStatsInterval
	}
	return &QueueStats{
		inspector: asynq.NewInspector(opt),
		interval:  interval,
		log:       log,
	}
}

// Run блокируется до отмены ctx (запускать горутиной из cmd/server).
// Недоступный Redis не роняет поллер — следующий тик попробует снова.
func (s *QueueStats) Run(ctx context.Context) {
	defer func() {
		if err := s.inspector.Close(); err != nil {
			s.log.Warn("metrics: inspector close", "error", err)
		}
	}()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scrape()
		}
	}
}

func (s *QueueStats) scrape() {
	queues, err := s.inspector.Queues()
	if err != nil {
		s.log.Warn("metrics: список очередей asynq недоступен", "error", err)
		return
	}
	for _, q := range queues {
		info, err := s.inspector.GetQueueInfo(q)
		if err != nil {
			s.log.Warn("metrics: статистика очереди недоступна", "queue", q, "error", err)
			continue
		}
		AsynqQueueSize.WithLabelValues(q, "pending").Set(float64(info.Pending))
		AsynqQueueSize.WithLabelValues(q, "active").Set(float64(info.Active))
		AsynqQueueSize.WithLabelValues(q, "scheduled").Set(float64(info.Scheduled))
		AsynqQueueSize.WithLabelValues(q, "retry").Set(float64(info.Retry))
		// В asynq это Archived; SRS §6.3 зовёт состояние dead — label
		// следует SRS, на него завязан алерт DeadLetterQueueGrowing.
		AsynqQueueSize.WithLabelValues(q, "dead").Set(float64(info.Archived))
	}
}
