// Интеграционные тесты воркера: настоящий Redis + Asynq (как queue_test в M2 —
// гоняются только при REDIS_TEST_ADDR, в CI он задан).
package worker

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
)

func testRedis(t *testing.T) config.RedisConfig {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR не задан — интеграционный тест воркера пропущен")
	}
	return config.RedisConfig{Addr: addr}
}

// cleanQueues прибирает все состояния очереди default между тестами.
func cleanQueues(t *testing.T, addr string) {
	t.Helper()
	insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: addr})
	defer insp.Close()
	for name, fn := range map[string]func(string) (int, error){
		"pending":   insp.DeleteAllPendingTasks,
		"scheduled": insp.DeleteAllScheduledTasks,
		"retry":     insp.DeleteAllRetryTasks,
		"archived":  insp.DeleteAllArchivedTasks,
		"completed": insp.DeleteAllCompletedTasks,
	} {
		if _, err := fn("default"); err != nil && !errors.Is(err, asynq.ErrQueueNotFound) {
			t.Logf("очистка %s: %v", name, err)
		}
	}
	// DeleteTask НЕ снимает unique-замки asynq (TTL 1 час) — без их зачистки
	// повторный прогон падает ErrDuplicate на первом же enqueue.
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	ctx := context.Background()
	keys, err := rdb.Keys(ctx, "asynq:{default}:unique:*").Result()
	if err != nil {
		t.Logf("поиск unique-замков: %v", err)
		return
	}
	if len(keys) > 0 {
		if err := rdb.Del(ctx, keys...).Err(); err != nil {
			t.Logf("зачистка unique-замков: %v", err)
		}
	}
}

// waitFor опрашивает cond до истечения timeout.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("не дождались: %s", what)
}

// TestWorker_1000Duplicates_OneExecution — стресс-критерий AQ²-6:
// 1000 дублей задачи → РОВНО одно выполнение.
func TestWorker_1000Duplicates_OneExecution(t *testing.T) {
	redisCfg := testRedis(t)
	cleanQueues(t, redisCfg.Addr)

	leads := newFakeLeads(testLead)
	msgs := &fakeMsgs{history: []models.Message{
		{LeadID: testLead.ID, Direction: models.DirectionInbound, Content: "привет"},
	}}
	ai := &fakeAI{reply: "здравствуйте"}
	snd := &fakeSender{}

	client := queue.NewClient(redisCfg)
	defer client.Close()

	// 1000 дублей одного telegram-апдейта: enqueue обязан пройти один раз,
	// остальные 999 — ErrDuplicate (TaskID+Unique, CLAUDE.md §4.5).
	ctx := context.Background()
	enqueued, dups := 0, 0
	for i := 0; i < 1000; i++ {
		switch err := client.EnqueueInbound(ctx, testLead.ID, 555); {
		case err == nil:
			enqueued++
		case errors.Is(err, queue.ErrDuplicate):
			dups++
		default:
			t.Fatalf("enqueue #%d: %v", i, err)
		}
	}
	if enqueued != 1 || dups != 999 {
		t.Fatalf("enqueued=%d dups=%d, ожидали 1/999", enqueued, dups)
	}

	srv := New(redisCfg,
		newTestProcessor(t, leads, msgs, ai, snd),
		newTestSummarizer(leads, msgs, &fakeSummaries{}, ai),
		snd, 0, testLogger())
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Shutdown()

	waitFor(t, 5*time.Second, func() bool { return snd.sentCount() >= 1 }, "обработка задачи")
	// Даём воркеру шанс совершить лишние выполнения, если бы они были.
	time.Sleep(300 * time.Millisecond)

	if got := ai.callCount(); got != 1 {
		t.Errorf("claude вызван %d раз, ожидали 1 (AQ²-6)", got)
	}
	if got := snd.sentCount(); got != 1 {
		t.Errorf("отправок лиду %d, ожидали 1 (AQ²-6)", got)
	}
}

// TestWorker_ClaudeDown_RetriesThenDeadLetterAndAlert — критерий §6.3:
// падение Claude → 3 ретрая → архив (dead letter) → алерт менеджеру.
func TestWorker_ClaudeDown_RetriesThenDeadLetterAndAlert(t *testing.T) {
	redisCfg := testRedis(t)
	cleanQueues(t, redisCfg.Addr)

	const managerChatID = int64(777001)

	leads := newFakeLeads(testLead)
	msgs := &fakeMsgs{history: []models.Message{
		{LeadID: testLead.ID, Direction: models.DirectionInbound, Content: "привет"},
	}}
	ai := &fakeAI{err: errors.New("claude api down")} // Claude лежит всегда
	snd := &fakeSender{}

	client := queue.NewClient(redisCfg)
	defer client.Close()
	if err := client.EnqueueInbound(context.Background(), testLead.ID, 556); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Боевой backoff 2/8/32 с растянул бы тест на 42+ секунды — ужимаем.
	// DelayedTaskCheckInterval тоже: по умолчанию asynq возвращает созревшие
	// ретраи в очередь раз в 5 с, три ретрая не влезли бы в таймаут теста.
	srv := New(redisCfg,
		newTestProcessor(t, leads, msgs, ai, snd),
		newTestSummarizer(leads, msgs, &fakeSummaries{}, ai),
		snd, managerChatID, testLogger(),
		WithRetryDelayFunc(func(int, error, *asynq.Task) time.Duration {
			return 50 * time.Millisecond
		}),
		func(c *asynq.Config) { c.DelayedTaskCheckInterval = 100 * time.Millisecond })
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Shutdown()

	insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: redisCfg.Addr})
	defer insp.Close()

	// Ретраи исчерпаны → задача в архиве asynq (dead letter §6.3).
	waitFor(t, 10*time.Second, func() bool {
		tasks, err := insp.ListArchivedTasks("default")
		return err == nil && len(tasks) == 1
	}, "задача в архиве (dead letter)")

	// 1 попытка + 3 ретрая (MaxRetry из M2).
	if got := ai.callCount(); got != 4 {
		t.Errorf("попыток %d, ожидали 4 (1 + 3 ретрая §6.3)", got)
	}

	// Алерт менеджеру в Telegram.
	waitFor(t, 3*time.Second, func() bool { return snd.sentCount() >= 1 }, "алерт менеджеру")
	snd.mu.Lock()
	defer snd.mu.Unlock()
	if len(snd.sent) != 1 {
		t.Fatalf("отправок %d, ожидали 1 (только алерт)", len(snd.sent))
	}
	alert := snd.sent[0]
	if alert.chatID != managerChatID {
		t.Errorf("алерт ушёл в чат %d, ожидали менеджера %d", alert.chatID, managerChatID)
	}
	if !strings.Contains(alert.text, queue.TypeProcessInbound) ||
		!strings.Contains(alert.text, "dead letter") {
		t.Errorf("текст алерта неинформативен: %q", alert.text)
	}
}

func mustBudgeter(t *testing.T) *Budgeter {
	t.Helper()
	b, err := NewBudgeter(budgetConfig(), &fakeCounter{})
	if err != nil {
		t.Fatalf("NewBudgeter: %v", err)
	}
	return b
}
