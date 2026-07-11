// emmakb.go — задача emma:kb:index (EP-03): асинхронная RAG-индексация
// файла базы знаний панели Эммы. Индексация ТОЛЬКО через очередь: Voyage
// free tier (3 RPM / 10K TPM) отвечает 429, ретраи с backoff прикрывают.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hibiken/asynq"
)

// TypeEmmaKBIndex — тип задачи «проиндексировать файл базы знаний»
// (контракт EP-03 наружу).
const TypeEmmaKBIndex = "emma:kb:index"

// kbMaxRetry — 5 попыток (task EP-03 §5, больше стандартных трёх):
// на бесплатном tier'е Voyage 429 — норма, а не сбой.
const kbMaxRetry = 5

// EmmaKBIndexPayload — полезная нагрузка emma:kb:index (контракт EP-03).
type EmmaKBIndexPayload struct {
	FileID int64 `json:"file_id"`
}

// KBIndexEnqueuer — контракт постановки индексации для ручек панели
// (в тестах — фейк).
type KBIndexEnqueuer interface {
	// EnqueueKBIndex ставит emma:kb:index. version — unix-время
	// загрузки/reindex: TaskID версионный (CLAUDE.md §4.5 + грабля M3 —
	// unique-замок не снимается DeleteTask, статический TaskID запер бы
	// reindex до конца Unique-окна). ErrDuplicate — эта же версия уже
	// в очереди (двойной сабмит формы).
	EnqueueKBIndex(ctx context.Context, fileID, version int64) error
}

func (c *Client) EnqueueKBIndex(ctx context.Context, fileID, version int64) error {
	payload, err := json.Marshal(EmmaKBIndexPayload{FileID: fileID})
	if err != nil {
		return fmt.Errorf("queue: marshal kb index payload: %w", err)
	}

	task := asynq.NewTask(TypeEmmaKBIndex, payload)
	_, err = c.c.EnqueueContext(ctx, task,
		asynq.TaskID(fmt.Sprintf("%s:%d:%d", TypeEmmaKBIndex, fileID, version)), // CLAUDE.md §4.5
		asynq.Unique(uniqueTTL),    // CLAUDE.md §4.5
		asynq.MaxRetry(kbMaxRetry), // task EP-03 §5
	)
	switch {
	case errors.Is(err, asynq.ErrTaskIDConflict), errors.Is(err, asynq.ErrDuplicateTask):
		return ErrDuplicate
	case err != nil:
		return fmt.Errorf("queue: enqueue %s: %w", TypeEmmaKBIndex, err)
	}
	return nil
}
