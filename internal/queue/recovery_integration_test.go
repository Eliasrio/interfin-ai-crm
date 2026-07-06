package queue

// Интеграционный тест критерия приёмки M11 №2: «Redis down → сообщения не
// теряются, recovery восстанавливает очередь». Требует REDIS_TEST_ADDR
// (как остальные интеграционные тесты очереди), иначе skip.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/models"
)

// pendingRepoStub — состояние leads.pending_task в памяти (сообщение уже
// в Postgres — это гарантирует вебхук M2 ДО ответа 200; здесь моделируем
// только флаг).
type pendingRepoStub struct {
	lead    models.Lead
	pending bool
}

func (s *pendingRepoStub) ListPendingTask(context.Context, int) ([]models.Lead, error) {
	if !s.pending {
		return nil, nil
	}
	return []models.Lead{s.lead}, nil
}

func (s *pendingRepoStub) ClearPendingTask(_ context.Context, id int64, seen int) (bool, error) {
	if s.pending && s.lead.ID == id && s.lead.MessageCount == seen {
		s.pending = false
		return true, nil
	}
	return false, nil
}

func TestRecovery_RedisDownThenUp_QueueRestored(t *testing.T) {
	addr := testRedis(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	inspector := asynq.NewInspector(asynq.RedisClientOpt{Addr: addr})
	defer inspector.Close()
	if _, err := inspector.DeleteAllPendingTasks("default"); err != nil &&
		!errors.Is(err, asynq.ErrQueueNotFound) {
		t.Logf("очистка очереди перед тестом: %v", err)
	}
	cleanUniqueLocks(t, addr)

	const leadID, msgCount = 77, 4
	repo := &pendingRepoStub{
		lead: models.Lead{ID: leadID, MessageCount: msgCount, PendingTask: true},
	}

	// Фаза 1: Redis «лежит» — клиент смотрит в порт, где никто не слушает.
	// Enqueue падает; вебхук в этот момент уже сохранил сообщение в БД и
	// выставил pending_task=TRUE (телеграм-хендлер, tested in M2).
	deadClient := NewClient(config.RedisConfig{Addr: "127.0.0.1:1"})
	if err := deadClient.EnqueueInbound(ctx, leadID, 1001); err == nil {
		t.Fatal("enqueue в мёртвый Redis обязан вернуть ошибку")
	}
	repo.pending = true // так поступает вебхук (SaveAndReturn200)

	// Recovery на мёртвом Redis флаг не снимает — сообщение не потеряно.
	NewRecovery(repo, deadClient, 0, log).Sweep(ctx)
	if !repo.pending {
		t.Fatal("pending_task снят, пока Redis лежит")
	}
	_ = deadClient.Close()

	// Фаза 2: Redis «ожил» — recovery перевыставляет задачу и снимает флаг.
	liveClient := NewClient(config.RedisConfig{Addr: addr})
	defer liveClient.Close()
	NewRecovery(repo, liveClient, 0, log).Sweep(ctx)

	if repo.pending {
		t.Fatal("pending_task не снят после восстановления Redis")
	}
	// Задача реально в очереди и адресуема контрактным ключом (-message_count).
	info, err := inspector.GetTaskInfo("default", DedupKey(leadID, -msgCount))
	if err != nil {
		t.Fatalf("восстановленная задача не найдена в очереди: %v", err)
	}
	if info.Type != TypeProcessInbound {
		t.Errorf("тип восстановленной задачи %q, ждали %q", info.Type, TypeProcessInbound)
	}

	// Повторный Sweep того же состояния не плодит вторую задачу (§4.5).
	repo.pending = true
	NewRecovery(repo, liveClient, 0, log).Sweep(ctx)
	tasks, err := inspector.ListPendingTasks("default")
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("в очереди %d задач, ждали 1 (ErrDuplicate обязан гасить повтор)", len(tasks))
	}
	if repo.pending {
		t.Error("после ErrDuplicate флаг обязан сняться (задача уже в очереди)")
	}

	if _, err := inspector.DeleteAllPendingTasks("default"); err != nil {
		t.Logf("очистка после теста: %v", err)
	}
	cleanUniqueLocks(t, addr)
}
