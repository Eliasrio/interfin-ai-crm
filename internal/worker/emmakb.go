// emmakb.go — обработчик emma:kb:index (EP-03): оригинал с диска →
// извлечение текста (kbtext) → rag.Indexer.IndexDocument → статус в
// emma_kb_files + WS-событие emma_kb_status на финале.
//
// Дисциплина ретраев (task EP-03 §5):
//   - ошибка извлечения (битый файл, скан без текстового слоя) перманентна —
//     status error сразу, asynq.SkipRetry;
//   - ошибка Voyage (429 free tier) или БД — временная: вернуть err, задача
//     ретраится (MaxRetry 5, backoff 2/8/32), статус остаётся pending;
//     на финальной неудачной попытке — status error, иначе файл завис бы
//     в pending навсегда.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/kbtext"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// KBIndexer — контракт на rag.Indexer (в тестах — фейк с фейковым Embedder).
type KBIndexer interface {
	IndexDocument(ctx context.Context, source, text string) (int, error)
}

// EmmaKBDeps — зависимости обработчика индексации базы знаний.
type EmmaKBDeps struct {
	Files   repo.EmmaKBRepo
	Indexer KBIndexer
	Pub     events.Publisher
	// EP-06: журнал emma_events (error/kb_index на финальной ошибке
	// индексации) и алерты владельцу (каждая ошибка kb_index — алерт,
	// с общим анти-шумом 15 мин). Оба nil — режим тестов EP-03.
	Events repo.EmmaEventsRepo
	Alerts AlertSink
	Log    *slog.Logger
}

// EmmaKBHandlers — обработчик emma:kb:index (регистрация — RegisterEmmaKB).
type EmmaKBHandlers struct {
	deps EmmaKBDeps
}

func NewEmmaKBHandlers(deps EmmaKBDeps) *EmmaKBHandlers {
	return &EmmaKBHandlers{deps: deps}
}

// HandleEmmaKBIndex — handler задачи emma:kb:index.
func (h *EmmaKBHandlers) HandleEmmaKBIndex(ctx context.Context, t *asynq.Task) error {
	var p queue.EmmaKBIndexPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("worker: payload emma:kb:index не разобран: %v: %w", err, asynq.SkipRetry)
	}
	log := h.deps.Log.With("task", queue.TypeEmmaKBIndex, "file_id", p.FileID)

	f, err := h.deps.Files.GetByID(ctx, p.FileID)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		// Файл удалили, пока задача стояла в очереди, — работы нет.
		log.Info("worker: kb-файл удалён до индексации, задача пропущена")
		return nil
	case err != nil:
		return fmt.Errorf("worker: kb-файл %d не прочитан из БД: %w", p.FileID, err)
	}

	data, err := os.ReadFile(f.FilePath)
	if err != nil {
		// Оригинала нет на диске — ретрай не поможет (файл появится только
		// повторной загрузкой, а она поставит новую задачу).
		return h.fail(ctx, f, fmt.Sprintf("оригинал не прочитан с диска: %v", err), log)
	}

	text, err := kbtext.Extract(f.MimeType, data)
	if err != nil {
		// Перманентно: битый файл / скан без текстового слоя (ТЗ §3).
		return h.fail(ctx, f, err.Error(), log)
	}

	chunks, err := h.deps.Indexer.IndexDocument(ctx, models.EmmaKBSource(f.ID), text)
	if err != nil {
		if isFinalAttempt(ctx) {
			// Ретраи исчерпаны — фиксируем error, файл не виснет в pending.
			h.setStatus(ctx, f, models.EmmaKBError, 0,
				ptr("индексация не удалась: "+err.Error()), log)
			h.recordIndexError(ctx, f, "индексация не удалась: "+err.Error(), log)
		}
		return fmt.Errorf("worker: индексация kb-файла %d: %w", f.ID, err)
	}

	// Статус не записался (БД мигнула) — вернуть err и переиндексировать
	// заново: IndexDocument идемпотентен (ReplaceSource).
	if err := h.deps.Files.SetStatus(ctx, f.ID, models.EmmaKBIndexed, chunks, nil); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			log.Info("worker: kb-файл удалён во время индексации")
			return nil
		}
		return fmt.Errorf("worker: статус indexed kb-файла %d: %w", f.ID, err)
	}
	h.publish(ctx, f, models.EmmaKBIndexed, chunks, nil, log)
	log.Info("worker: kb-файл проиндексирован", "chunks", chunks)
	return nil
}

// fail — перманентная ошибка: status error + WS-событие + SkipRetry.
func (h *EmmaKBHandlers) fail(ctx context.Context, f *models.EmmaKBFile, msg string, log *slog.Logger) error {
	h.setStatus(ctx, f, models.EmmaKBError, 0, &msg, log)
	h.recordIndexError(ctx, f, msg, log)
	log.Warn("worker: kb-файл не проиндексирован (без ретрая)", "reason", msg)
	return fmt.Errorf("worker: kb-файл %d: %s: %w", f.ID, msg, asynq.SkipRetry)
}

// recordIndexError — EP-06: финальный error индексации → событие
// error/kb_index (detail — имя файла + причина, task §1) + алерт владельцу
// (каждая ошибка kb_index, task §5). Best effort: журнал и алерты не
// влияют ни на статус файла, ни на дисциплину ретраев.
func (h *EmmaKBHandlers) recordIndexError(ctx context.Context, f *models.EmmaKBFile, reason string, log *slog.Logger) {
	kind := models.EmmaErrKBIndex
	detail := truncateDetail(f.Filename + ": " + reason)
	if h.deps.Events != nil {
		ev := &models.EmmaEvent{EventType: models.EmmaEventError, ErrorKind: &kind, Detail: &detail}
		if err := h.deps.Events.Create(ctx, ev); err != nil {
			log.Warn("worker: событие kb_index не записано", "error", err)
		}
	}
	if h.deps.Alerts != nil {
		h.deps.Alerts.OnError(ctx, kind, detail)
	}
}

// setStatus — запись статуса + WS-событие; ошибки только в лог (мы уже
// на ветке финала, ронять задачу поздно и не за чем).
func (h *EmmaKBHandlers) setStatus(ctx context.Context, f *models.EmmaKBFile, status string, chunks int, errText *string, log *slog.Logger) {
	if err := h.deps.Files.SetStatus(ctx, f.ID, status, chunks, errText); err != nil {
		log.Error("worker: статус kb-файла не записан", "status", status, "error", err)
		return
	}
	h.publish(ctx, f, status, chunks, errText, log)
}

// publish — WS-событие emma_kb_status (fire-and-forget, как весь канал
// crm:events: потерю фронт добирает GET-списком).
func (h *EmmaKBHandlers) publish(ctx context.Context, f *models.EmmaKBFile, status string, chunks int, errText *string, log *slog.Logger) {
	ev := events.EmmaKBStatusEvent(f.ID, f.Filename, status, chunks, errText)
	if err := h.deps.Pub.Publish(ctx, ev); err != nil {
		log.Warn("worker: событие emma_kb_status не опубликовано", "error", err)
	}
}

// isFinalAttempt — текущая попытка последняя (ретраи будут исчерпаны).
func isFinalAttempt(ctx context.Context) bool {
	retried, okR := asynq.GetRetryCount(ctx)
	maxRetry, okM := asynq.GetMaxRetry(ctx)
	return okR && okM && retried >= maxRetry
}

// ptr — указатель на строку (nullable index_error).
func ptr(s string) *string { return &s }
