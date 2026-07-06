package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/config"
)

func TestDedupKey(t *testing.T) {
	// Контракт M2: sha256(lead_id:msg_id) в hex.
	want := sha256.Sum256([]byte("42:1001"))
	if got := DedupKey(42, 1001); got != hex.EncodeToString(want[:]) {
		t.Errorf("DedupKey(42,1001) = %s, ожидали sha256(\"42:1001\")", got)
	}

	if DedupKey(42, 1001) != DedupKey(42, 1001) {
		t.Error("дедуп-ключ обязан быть детерминированным")
	}
	if DedupKey(42, 1001) == DedupKey(42, 1002) || DedupKey(42, 1001) == DedupKey(43, 1001) {
		t.Error("разные (lead_id, msg_id) должны давать разные ключи")
	}
}

// testRedis возвращает адрес тестового Redis или скипает тест —
// та же схема, что POSTGRES_TEST_DSN в интеграционных тестах M1.
func testRedis(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR не задан — интеграционный тест очереди пропущен")
	}
	return addr
}

// TestEnqueueInbound_DuplicateGivesOneTask — критерий приёмки AQ²-6:
// повторная доставка того же апдейта даёт РОВНО одну задачу в очереди.
func TestEnqueueInbound_DuplicateGivesOneTask(t *testing.T) {
	addr := testRedis(t)

	inspector := asynq.NewInspector(asynq.RedisClientOpt{Addr: addr})
	defer inspector.Close()
	// Чистим следы прошлых прогонов, чтобы счёт задач был честным.
	// На свежем Redis очереди ещё нет — это не ошибка.
	if _, err := inspector.DeleteAllPendingTasks("default"); err != nil &&
		!errors.Is(err, asynq.ErrQueueNotFound) {
		t.Logf("очистка очереди перед тестом: %v", err)
	}

	c := NewClient(config.RedisConfig{Addr: addr})
	defer c.Close()

	ctx := context.Background()
	const leadID, msgID = 42, 1001

	if err := c.EnqueueInbound(ctx, leadID, msgID); err != nil {
		t.Fatalf("первый enqueue: %v", err)
	}
	// Дубль того же апдейта.
	if err := c.EnqueueInbound(ctx, leadID, msgID); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("второй enqueue: ожидали ErrDuplicate, получили %v", err)
	}
	// Другое сообщение того же лида — отдельная задача.
	if err := c.EnqueueInbound(ctx, leadID, msgID+1); err != nil {
		t.Fatalf("enqueue другого сообщения: %v", err)
	}

	tasks, err := inspector.ListPendingTasks("default")
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("в очереди %d задач, ожидали 2 (дубль не должен добавлять третью)", len(tasks))
	}
	for _, task := range tasks {
		if task.Type != TypeProcessInbound {
			t.Errorf("неожиданный тип задачи %q", task.Type)
		}
	}

	// Задача дубля адресуема по контрактному ключу.
	info, err := inspector.GetTaskInfo("default", DedupKey(leadID, msgID))
	if err != nil {
		t.Fatalf("задача с дедуп-ключом не найдена: %v", err)
	}
	wantPayload := fmt.Sprintf(`{"lead_id":%d,"msg_id":%d}`, leadID, msgID)
	if string(info.Payload) != wantPayload {
		t.Errorf("payload = %s, ожидали %s (контракт M2→M3)", info.Payload, wantPayload)
	}

	// Прибираем за собой.
	if _, err := inspector.DeleteAllPendingTasks("default"); err != nil {
		t.Logf("очистка после теста: %v", err)
	}
}
