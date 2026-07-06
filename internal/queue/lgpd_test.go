// Интеграционный тест дедупа lgpd:retention (CLAUDE.md §4.5): повторная
// постановка в тот же день — ErrDuplicate, в очереди одна задача.
// Запуск: REDIS_TEST_ADDR=localhost:6379 go test ./internal/queue
package queue

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/config"
)

func TestRetentionEnqueueDedup(t *testing.T) {
	addr := testRedis(t)
	inspector := asynq.NewInspector(asynq.RedisClientOpt{Addr: addr})
	defer inspector.Close()

	// Чистый старт: сегодняшняя задача и unique-замки от прошлых прогонов.
	taskID := TypeLGPDRetention + ":" + time.Now().UTC().Format("2006-01-02")
	if err := inspector.DeleteTask("default", taskID); err != nil &&
		!errors.Is(err, asynq.ErrTaskNotFound) {
		t.Logf("зачистка перед тестом: %v", err)
	}
	cleanUniqueLocks(t, addr)

	sched, err := NewRetentionScheduler(
		config.RedisConfig{Addr: addr},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer sched.Shutdown()

	ctx := context.Background()
	if err := sched.EnqueueRetention(ctx); err != nil {
		t.Fatalf("первая постановка: %v", err)
	}
	// Вторая постановка того же дня (другая «реплика») — дубль.
	if err := sched.EnqueueRetention(ctx); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("повторная постановка: %v, ждали ErrDuplicate", err)
	}

	// В очереди ровно одна задача с датированным TaskID.
	info, err := inspector.GetTaskInfo("default", taskID)
	if err != nil {
		t.Fatalf("задача %s не найдена: %v", taskID, err)
	}
	if info.Type != TypeLGPDRetention {
		t.Fatalf("тип задачи %s, ждали %s", info.Type, TypeLGPDRetention)
	}

	// Прибираем за собой (замок переживает DeleteTask — чистим отдельно).
	if err := inspector.DeleteTask("default", taskID); err != nil {
		t.Logf("очистка после теста: %v", err)
	}
	cleanUniqueLocks(t, addr)
}
