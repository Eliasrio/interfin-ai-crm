// summary_test.go — обработчик summary:generate (M4 §7.3).
// Юнит-сценарии — на fakeLocker; критерий IQ-5 (1000 конкурентов → ровно
// 1 summary) — на настоящем Redis (REDIS_TEST_ADDR, как прочие интеграционные).
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/interfin/interfin-ai-crm/internal/claude"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
)

func summaryTask(t *testing.T, leadID int64, messageCount int) *asynq.Task {
	t.Helper()
	payload, err := json.Marshal(queue.SummaryPayload{LeadID: leadID, MessageCount: messageCount})
	if err != nil {
		t.Fatal(err)
	}
	return asynq.NewTask(queue.TypeSummaryGenerate, payload)
}

func dialogHistory(n int) *fakeMsgs {
	msgs := &fakeMsgs{}
	for i := 0; i < n; i++ {
		dir := models.DirectionInbound
		if i%2 == 1 {
			dir = models.DirectionOutbound
		}
		msgs.history = append(msgs.history, models.Message{LeadID: 7, Direction: dir, Content: "реплика"})
	}
	return msgs
}

func TestSummary_GeneratesAndStores(t *testing.T) {
	sums := &fakeSummaries{}
	ai := &fakeAI{reply: "Клиент спрашивал о тарифах, бюджет не назван."}
	locker := &fakeLocker{}
	s := NewSummarizer(newFakeLeads(testLead), dialogHistory(15), sums, ai, locker, testLogger())

	if err := s.HandleSummaryGenerate(context.Background(), summaryTask(t, 7, 15)); err != nil {
		t.Fatal(err)
	}
	got, err := sums.GetByLead(context.Background(), 7)
	if err != nil {
		t.Fatalf("сводка не записана: %v", err)
	}
	if got.Content != ai.reply || got.MessageCount != 15 {
		t.Errorf("сводка: %+v", got)
	}
	// DEL после записи (§7.3): замок свободен.
	if free, _ := locker.TryLock(context.Background(), summaryLockKey(7), time.Minute); !free {
		t.Error("замок не снят после записи сводки")
	}
}

func TestSummary_LockBusySkips(t *testing.T) {
	// Конкурент держит замок → nil (skip §7.3), Claude не вызывается.
	locker := &fakeLocker{}
	if ok, _ := locker.TryLock(context.Background(), summaryLockKey(7), time.Minute); !ok {
		t.Fatal("setup: замок не взят")
	}
	ai := &fakeAI{reply: "x"}
	sums := &fakeSummaries{}
	s := NewSummarizer(newFakeLeads(testLead), dialogHistory(15), sums, ai, locker, testLogger())

	if err := s.HandleSummaryGenerate(context.Background(), summaryTask(t, 7, 15)); err != nil {
		t.Fatalf("занятый замок = штатный skip, не ошибка: %v", err)
	}
	if ai.callCount() != 0 || sums.upsertCount() != 0 {
		t.Error("при занятом замке не должно быть ни Claude, ни записи")
	}
}

func TestSummary_AlreadyFreshSkips(t *testing.T) {
	// Конкурент уже записал сводку этого блока, пока задача ждала в очереди.
	sums := &fakeSummaries{data: map[int64]models.ConversationSummary{
		7: {LeadID: 7, Content: "свежая", MessageCount: 15},
	}}
	ai := &fakeAI{reply: "x"}
	s := NewSummarizer(newFakeLeads(testLead), dialogHistory(15), sums, ai, &fakeLocker{}, testLogger())

	if err := s.HandleSummaryGenerate(context.Background(), summaryTask(t, 7, 15)); err != nil {
		t.Fatal(err)
	}
	if ai.callCount() != 0 {
		t.Error("сводка уже актуальна — Claude звать незачем")
	}
}

func TestSummary_NextBlockIncludesPreviousSummary(t *testing.T) {
	// 30-е сообщение: прошлая сводка уходит в контекст генерации (история
	// ограничена historyFetchLimit — старые реплики живут только в сводке).
	sums := &fakeSummaries{data: map[int64]models.ConversationSummary{
		7: {LeadID: 7, Content: "клиент выбирал тариф", MessageCount: 15},
	}}
	var gotDialog string
	ai := &recordingAI{onComplete: func(_ string, msgs []claude.Message) string {
		gotDialog = msgs[0].Content
		return "обновлённая сводка"
	}}
	s := NewSummarizer(newFakeLeads(testLead), dialogHistory(30), sums, ai, &fakeLocker{}, testLogger())

	if err := s.HandleSummaryGenerate(context.Background(), summaryTask(t, 7, 30)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotDialog, "клиент выбирал тариф") {
		t.Error("прошлая сводка не попала в контекст генерации")
	}
	got, _ := sums.GetByLead(context.Background(), 7)
	if got == nil || got.MessageCount != 30 || got.Content != "обновлённая сводка" {
		t.Errorf("сводка после обновления: %+v", got)
	}
}

func TestSummary_LeadErasedSkips(t *testing.T) {
	ai := &fakeAI{reply: "x"}
	s := NewSummarizer(newFakeLeads(), dialogHistory(15), &fakeSummaries{}, ai, &fakeLocker{}, testLogger())
	if err := s.HandleSummaryGenerate(context.Background(), summaryTask(t, 7, 15)); err != nil {
		t.Fatalf("стёртый лид = skip, не ретрай: %v", err)
	}
	if ai.callCount() != 0 {
		t.Error("для стёртого лида Claude не вызывается")
	}
}

func TestSummary_ClaudeErrorRetriesAndReleasesLock(t *testing.T) {
	ai := &fakeAI{err: errors.New("api 529")}
	locker := &fakeLocker{}
	s := NewSummarizer(newFakeLeads(testLead), dialogHistory(15), &fakeSummaries{}, ai, locker, testLogger())

	if err := s.HandleSummaryGenerate(context.Background(), summaryTask(t, 7, 15)); err == nil {
		t.Fatal("ошибка Claude обязана уходить в ретрай Asynq")
	}
	// Замок снят: ретраю не нужно ждать TTL 60 с.
	if free, _ := locker.TryLock(context.Background(), summaryLockKey(7), time.Minute); !free {
		t.Error("замок не снят после ошибки генерации")
	}
}

func TestSummary_MalformedPayloadSkipsRetry(t *testing.T) {
	s := NewSummarizer(newFakeLeads(testLead), dialogHistory(1), &fakeSummaries{}, &fakeAI{}, &fakeLocker{}, testLogger())
	err := s.HandleSummaryGenerate(context.Background(),
		asynq.NewTask(queue.TypeSummaryGenerate, []byte("не json")))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("ожидали SkipRetry, получили %v", err)
	}
}

// recordingAI — Completer с перехватом аргументов (для проверки контекста).
type recordingAI struct {
	mu         sync.Mutex
	calls      int
	onComplete func(system string, msgs []claude.Message) string
}

func (r *recordingAI) Complete(_ context.Context, system string, msgs []claude.Message) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.onComplete(system, msgs), nil
}

// slowAI — Completer с задержкой: удерживает «генерацию» достаточно долго,
// чтобы конкуренты IQ-5 успели упереться в замок.
type slowAI struct {
	inner *fakeAI
	delay time.Duration
}

func (s *slowAI) Complete(ctx context.Context, system string, msgs []claude.Message) (string, error) {
	time.Sleep(s.delay)
	return s.inner.Complete(ctx, system, msgs)
}

// TestSummary_IQ5_ThousandConcurrentWorkersOneSummary — критерий IQ-5:
// 1000 конкурентных воркеров на 15-м сообщении → РОВНО 1 summary
// (1 вызов Claude, 1 строка в БД). Настоящий Redis-замок (§7.3).
func TestSummary_IQ5_ThousandConcurrentWorkersOneSummary(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR не задан — интеграционный тест пропущен")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	ctx := context.Background()
	if err := rdb.Del(ctx, summaryLockKey(7)).Err(); err != nil {
		t.Fatalf("зачистка замка: %v", err)
	}

	sums := &fakeSummaries{}
	ai := &fakeAI{reply: "единственная сводка"}
	s := NewSummarizer(
		newFakeLeads(testLead),
		dialogHistory(15),
		sums,
		&slowAI{inner: ai, delay: 100 * time.Millisecond},
		NewRedisLocker(rdb),
		testLogger(),
	)

	const workers = 1000
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // одновременный старт всех воркеров
			errs <- s.HandleSummaryGenerate(ctx, summaryTask(t, 7, 15))
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("конкурентный обработчик вернул ошибку: %v", err)
		}
	}

	if got := ai.callCount(); got != 1 {
		t.Errorf("вызовов Claude: %d, ожидали ровно 1 (IQ-5)", got)
	}
	got, err := sums.GetByLead(ctx, 7)
	if err != nil {
		t.Fatalf("сводка не записана: %v", err)
	}
	if got.Content != "единственная сводка" || got.MessageCount != 15 {
		t.Errorf("сводка: %+v", got)
	}
	// Замок снят — следующий блок (count=30) не будет ждать TTL.
	if free, err := rdb.SetNX(ctx, summaryLockKey(7), 1, time.Second).Result(); err != nil || !free {
		t.Errorf("замок не снят после генерации: free=%v err=%v", free, err)
	}
	rdb.Del(ctx, summaryLockKey(7))
}
