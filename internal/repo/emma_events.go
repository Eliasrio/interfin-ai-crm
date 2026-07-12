// emma_events.go — журнал работы Эммы (таблица 0020). EP-04 пишет
// file_sent и error (file_not_found/telegram_api), EP-06 пишет reply
// и строит поверх статистику вкладки 6 (Stats/ListErrors/DialogStats).
package repo

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

// NewEmmaEvents — репозиторий emma_events поверх того же *gorm.DB.
func NewEmmaEvents(db *gorm.DB) EmmaEventsRepo {
	return &emmaEventsRepo{db: db}
}

// NewEmmaStats — читающая половина того же репозитория (вкладка 6, EP-06).
func NewEmmaStats(db *gorm.DB) EmmaStatsRepo {
	return &emmaEventsRepo{db: db}
}

type emmaEventsRepo struct{ db *gorm.DB }

func (r *emmaEventsRepo) Create(ctx context.Context, ev *models.EmmaEvent) error {
	if err := r.db.WithContext(ctx).Create(ev).Error; err != nil {
		return fmt.Errorf("repo: create emma event %s: %w", ev.EventType, err)
	}
	return nil
}

// errorsPageSize — журнал ошибок, строк на страницу (ТЗ §3: 50).
const errorsPageSize = 50

// Stats — агрегаты журнала за [from, to): одна выборка по счётчикам reply/
// handoff/file_sent (FILTER + percentile_cont — Postgres 16, SimpleProtocol
// не мешает), плюс две разбивки (файлы с именами, ошибки по kind).
func (r *emmaEventsRepo) Stats(ctx context.Context, from, to time.Time) (*EmmaStats, error) {
	var agg struct {
		Replies       int64
		AvgResponseMs float64
		P95ResponseMs float64
		TokensIn      int64
		TokensOut     int64
		Handoffs      int64
		FilesSent     int64
	}
	err := r.db.WithContext(ctx).Raw(`
		SELECT
		  count(*) FILTER (WHERE event_type = 'reply')                            AS replies,
		  COALESCE(avg(response_time_ms) FILTER (WHERE event_type = 'reply'), 0)  AS avg_response_ms,
		  COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY response_time_ms)
		           FILTER (WHERE event_type = 'reply' AND response_time_ms IS NOT NULL), 0) AS p95_response_ms,
		  COALESCE(sum(tokens_in)  FILTER (WHERE event_type = 'reply'), 0)        AS tokens_in,
		  COALESCE(sum(tokens_out) FILTER (WHERE event_type = 'reply'), 0)        AS tokens_out,
		  count(*) FILTER (WHERE event_type = 'handoff')                          AS handoffs,
		  count(*) FILTER (WHERE event_type = 'file_sent')                        AS files_sent
		FROM emma_events
		WHERE created_at >= ? AND created_at < ?`, from, to).Scan(&agg).Error
	if err != nil {
		return nil, fmt.Errorf("repo: emma stats aggregate: %w", err)
	}

	stats := &EmmaStats{
		Replies:       agg.Replies,
		AvgResponseMs: agg.AvgResponseMs,
		P95ResponseMs: agg.P95ResponseMs,
		TokensIn:      agg.TokensIn,
		TokensOut:     agg.TokensOut,
		Handoffs:      agg.Handoffs,
		FilesSent:     agg.FilesSent,
		ErrorsByKind:  map[string]int64{},
	}

	// Разбивка по файлам: LEFT JOIN — события файлов, удалённых из
	// библиотеки (SET NULL, 0020), собираются в одну строку без имени.
	var files []struct {
		SendFileID *int64
		Name       *string
		Count      int64
	}
	err = r.db.WithContext(ctx).Raw(`
		SELECT e.send_file_id, f.name, count(*) AS count
		FROM emma_events e
		LEFT JOIN emma_send_files f ON f.id = e.send_file_id
		WHERE e.event_type = 'file_sent' AND e.created_at >= ? AND e.created_at < ?
		GROUP BY e.send_file_id, f.name
		ORDER BY count DESC, f.name NULLS LAST`, from, to).Scan(&files).Error
	if err != nil {
		return nil, fmt.Errorf("repo: emma stats files: %w", err)
	}
	for _, f := range files {
		name := "(файл удалён)"
		if f.Name != nil {
			name = *f.Name
		}
		stats.Files = append(stats.Files, EmmaFileSentCount{
			SendFileID: f.SendFileID, Name: name, Count: f.Count,
		})
	}

	var errs []struct {
		Kind  string
		Count int64
	}
	err = r.db.WithContext(ctx).Raw(`
		SELECT COALESCE(error_kind, 'unknown') AS kind, count(*) AS count
		FROM emma_events
		WHERE event_type = 'error' AND created_at >= ? AND created_at < ?
		GROUP BY error_kind`, from, to).Scan(&errs).Error
	if err != nil {
		return nil, fmt.Errorf("repo: emma stats errors: %w", err)
	}
	for _, e := range errs {
		stats.ErrorsByKind[e.Kind] = e.Count
	}
	return stats, nil
}

// ListErrors — страница журнала ошибок (новые сверху, стабильный порядок
// по id при равных created_at). page < 1 читается как 1.
func (r *emmaEventsRepo) ListErrors(ctx context.Context, kind string, from, to time.Time, page int) ([]models.EmmaEvent, int64, error) {
	if page < 1 {
		page = 1
	}
	q := r.db.WithContext(ctx).Model(&models.EmmaEvent{}).
		Where("event_type = ?", models.EmmaEventError).
		Where("created_at >= ? AND created_at < ?", from, to)
	if kind != "" {
		q = q.Where("error_kind = ?", kind)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("repo: emma errors count: %w", err)
	}
	var events []models.EmmaEvent
	err := q.Order("created_at DESC, id DESC").
		Offset((page - 1) * errorsPageSize).Limit(errorsPageSize).
		Find(&events).Error
	if err != nil {
		return nil, 0, fmt.Errorf("repo: emma errors list: %w", err)
	}
	return events, total, nil
}

// DialogStats — метрики существующих leads/messages одной выборкой:
// новые лиды и сообщения за период, активные диалоги — с activeSince
// (последние 24 ч, окно НЕ зависит от периода — task §3).
func (r *emmaEventsRepo) DialogStats(ctx context.Context, from, to, activeSince time.Time) (*EmmaDialogStats, error) {
	var ds EmmaDialogStats
	err := r.db.WithContext(ctx).Raw(`
		SELECT
		  (SELECT count(*) FROM leads
		   WHERE created_at >= ? AND created_at < ?)                    AS new_leads,
		  (SELECT count(DISTINCT lead_id) FROM messages
		   WHERE direction = 'inbound' AND created_at >= ?)             AS active_dialogs,
		  (SELECT count(*) FROM messages
		   WHERE direction = 'inbound'
		     AND created_at >= ? AND created_at < ?)                    AS messages_in,
		  (SELECT count(*) FROM messages
		   WHERE direction = 'outbound'
		     AND created_at >= ? AND created_at < ?)                    AS messages_out`,
		from, to, activeSince, from, to, from, to).Scan(&ds).Error
	if err != nil {
		return nil, fmt.Errorf("repo: emma dialog stats: %w", err)
	}
	return &ds, nil
}
