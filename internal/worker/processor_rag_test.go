// processor_rag_test.go — сценарии M4: RAG-чанки в system prompt, fallback
// при rag_miss, инжект summary, триггер summary каждые 15 inbound.
package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// --- фейки M4 ---

type fakeRetriever struct {
	mu       sync.Mutex
	chunks   []repo.ScoredChunk
	err      error
	gotQuery string
	gotLead  int64
	calls    int
}

func (f *fakeRetriever) Retrieve(_ context.Context, leadID int64, query string) ([]repo.ScoredChunk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotLead = leadID
	f.gotQuery = query
	return f.chunks, f.err
}

type fakeSummaries struct {
	mu      sync.Mutex
	data    map[int64]models.ConversationSummary
	upserts int
	getErr  error
}

func (f *fakeSummaries) Upsert(_ context.Context, s *models.ConversationSummary) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.data == nil {
		f.data = map[int64]models.ConversationSummary{}
	}
	// Та же семантика свежести, что у боевого репозитория.
	if cur, ok := f.data[s.LeadID]; !ok || cur.MessageCount <= s.MessageCount {
		f.data[s.LeadID] = *s
	}
	f.upserts++
	return nil
}

func (f *fakeSummaries) GetByLead(_ context.Context, leadID int64) (*models.ConversationSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	s, ok := f.data[leadID]
	if !ok {
		return nil, repo.ErrNotFound
	}
	cp := s
	return &cp, nil
}

func (f *fakeSummaries) upsertCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upserts
}

type fakeSummaryEnq struct {
	mu    sync.Mutex
	calls []queue.SummaryPayload
	err   error
}

func (f *fakeSummaryEnq) EnqueueSummary(_ context.Context, leadID int64, messageCount int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, queue.SummaryPayload{LeadID: leadID, MessageCount: messageCount})
	return f.err
}

// fakeLocker — замок в памяти процесса (для юнит-тестов; IQ-5 гоняется
// на настоящем Redis в summary_test.go).
type fakeLocker struct {
	mu   sync.Mutex
	held map[string]bool
}

func (f *fakeLocker) TryLock(_ context.Context, key string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held == nil {
		f.held = map[string]bool{}
	}
	if f.held[key] {
		return false, nil
	}
	f.held[key] = true
	return true, nil
}

func (f *fakeLocker) Unlock(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.held, key)
	return nil
}

// newTestSummarizer — сумматор на фейковом замке (юнит-сценарии).
func newTestSummarizer(leads repo.LeadRepo, msgs repo.MessageRepo, sums repo.SummaryRepo, ai Completer) *Summarizer {
	return NewSummarizer(leads, msgs, sums, ai, &fakeLocker{}, testLogger())
}

// newRAGProcessor — процессор в полной комплектации M4.
func newRAGProcessor(
	t *testing.T,
	leads *fakeLeads, msgs *fakeMsgs, ai *fakeAI, snd *fakeSender,
	retr *fakeRetriever, sums *fakeSummaries, enq *fakeSummaryEnq,
) *Processor {
	t.Helper()
	return NewProcessor(ProcessorDeps{
		Leads:         leads,
		Msgs:          msgs,
		Budgeter:      mustBudgeter(t),
		AI:            ai,
		Sender:        snd,
		Retriever:     retr,
		Summaries:     sums,
		SummaryEnq:    enq,
		SummaryEveryN: 15,
		Log:           testLogger(),
	})
}

func inboundHistory(content string) *fakeMsgs {
	return &fakeMsgs{history: []models.Message{
		{LeadID: 7, Direction: models.DirectionInbound, Content: content},
	}}
}

// --- тесты ---

// TestHandle_RAGChunksInjected — найденные чанки уходят в system prompt (§7.1).
func TestHandle_RAGChunksInjected(t *testing.T) {
	retr := &fakeRetriever{chunks: []repo.ScoredChunk{
		{Source: "faq.md", Content: "Минимальный депозит — уточняется у менеджера.", Similarity: 0.9},
		{Source: "faq.md", Content: "Вывод средств занимает 1-3 дня.", Similarity: 0.82},
	}}
	ai := &fakeAI{reply: "ответ"}
	msgs := inboundHistory("какой минимальный депозит?")
	p := newRAGProcessor(t, newFakeLeads(testLead), msgs, ai, &fakeSender{}, retr, &fakeSummaries{}, &fakeSummaryEnq{})

	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatal(err)
	}

	if retr.gotLead != 7 || retr.gotQuery != "какой минимальный депозит?" {
		t.Errorf("retriever получил lead=%d query=%q", retr.gotLead, retr.gotQuery)
	}
	system := ai.lastSystem()
	if !strings.Contains(system, "Минимальный депозит") || !strings.Contains(system, "Вывод средств") {
		t.Errorf("system prompt без RAG-чанков:\n%s", system)
	}
	if !strings.Contains(system, "Эмма") {
		t.Error("база system prompt потерялась при инжекте RAG")
	}
}

// TestHandle_RAGMissFallsBack — 0 результатов → ответ без RAG (§7.1 fallback);
// диалог не падает, system prompt чистый.
func TestHandle_RAGMissFallsBack(t *testing.T) {
	retr := &fakeRetriever{chunks: nil} // rag_miss (аудит — забота ретривера)
	ai := &fakeAI{reply: "ответ без знаний"}
	snd := &fakeSender{}
	p := newRAGProcessor(t, newFakeLeads(testLead), inboundHistory("странный вопрос"),
		ai, snd, retr, &fakeSummaries{}, &fakeSummaryEnq{})

	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("rag_miss не должен ронять диалог: %v", err)
	}
	if snd.sentCount() != 1 {
		t.Fatal("ответ лиду не отправлен")
	}
	if strings.Contains(ai.lastSystem(), "Выдержки из базы знаний") {
		t.Errorf("при rag_miss RAG-секции в system быть не должно:\n%s", ai.lastSystem())
	}
}

// TestHandle_RetrieverErrorRetries — ошибка Voyage/БД → ошибка задачи
// (ретрай Asynq §6.3), Claude не вызывается.
func TestHandle_RetrieverErrorRetries(t *testing.T) {
	retr := &fakeRetriever{err: errors.New("voyage 500")}
	ai := &fakeAI{reply: "x"}
	p := newRAGProcessor(t, newFakeLeads(testLead), inboundHistory("вопрос"),
		ai, &fakeSender{}, retr, &fakeSummaries{}, &fakeSummaryEnq{})

	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err == nil {
		t.Fatal("ошибка ретривера обязана уходить в ретрай")
	}
	if ai.callCount() != 0 {
		t.Error("при ошибке ретривера Claude вызываться не должен")
	}
}

// TestHandle_SummaryInjected — сводка из БД уходит в контекст (§7.3, M4-6).
func TestHandle_SummaryInjected(t *testing.T) {
	sums := &fakeSummaries{data: map[int64]models.ConversationSummary{
		7: {LeadID: 7, Content: "Клиент интересуется стейкингом, бюджет ~10k USDT.", MessageCount: 15},
	}}
	ai := &fakeAI{reply: "ответ"}
	p := newRAGProcessor(t, newFakeLeads(testLead), inboundHistory("а что по срокам?"),
		ai, &fakeSender{}, &fakeRetriever{}, sums, &fakeSummaryEnq{})

	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatal(err)
	}
	system := ai.lastSystem()
	if !strings.Contains(system, "Сводка предыдущего диалога") ||
		!strings.Contains(system, "стейкингом") {
		t.Errorf("сводка не в контексте:\n%s", system)
	}
}

// TestHandle_SummaryEnqueuedOnEveryNth — задача summary ставится на каждом
// 15-м inbound и не ставится на прочих (§7.3).
func TestHandle_SummaryEnqueuedOnEveryNth(t *testing.T) {
	for _, tc := range []struct {
		count   int
		enqueue bool
	}{
		{count: 15, enqueue: true},
		{count: 30, enqueue: true},
		{count: 16, enqueue: false},
		{count: 1, enqueue: false},
	} {
		enq := &fakeSummaryEnq{}
		lead := &models.Lead{ID: 7, TelegramUserID: 424242, MessageCount: tc.count}
		p := newRAGProcessor(t, newFakeLeads(lead), inboundHistory("вопрос"),
			&fakeAI{reply: "ответ"}, &fakeSender{}, &fakeRetriever{}, &fakeSummaries{}, enq)

		if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
			t.Fatalf("count=%d: %v", tc.count, err)
		}
		if got := len(enq.calls); got != map[bool]int{true: 1, false: 0}[tc.enqueue] {
			t.Errorf("count=%d: %d постановок summary, ожидали enqueue=%v", tc.count, got, tc.enqueue)
		}
		if tc.enqueue && (enq.calls[0].LeadID != 7 || enq.calls[0].MessageCount != tc.count) {
			t.Errorf("payload: %+v", enq.calls[0])
		}
	}
}

// TestHandle_SummaryEnqueueDupIsFine — ErrDuplicate от очереди (ретрай задачи,
// дубль апдейта) не считается сбоем диалога.
func TestHandle_SummaryEnqueueDupIsFine(t *testing.T) {
	enq := &fakeSummaryEnq{err: queue.ErrDuplicate}
	lead := &models.Lead{ID: 7, TelegramUserID: 424242, MessageCount: 15}
	snd := &fakeSender{}
	p := newRAGProcessor(t, newFakeLeads(lead), inboundHistory("вопрос"),
		&fakeAI{reply: "ответ"}, snd, &fakeRetriever{}, &fakeSummaries{}, enq)

	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("дубль постановки summary не должен ронять диалог: %v", err)
	}
	if snd.sentCount() != 1 {
		t.Error("ответ лиду обязан уйти")
	}
}

// TestHandle_ResendBranchStillEnqueuesSummary — ветка переотправки (ретрай
// после падения Send) не теряет триггер summary.
func TestHandle_ResendBranchStillEnqueuesSummary(t *testing.T) {
	enq := &fakeSummaryEnq{}
	lead := &models.Lead{ID: 7, TelegramUserID: 424242, MessageCount: 15}
	msgs := &fakeMsgs{history: []models.Message{
		{LeadID: 7, Direction: models.DirectionInbound, Content: "вопрос"},
		{LeadID: 7, Direction: models.DirectionOutbound, Content: "сохранённый ответ"},
	}}
	ai := &fakeAI{reply: "не должен вызываться"}
	p := newRAGProcessor(t, newFakeLeads(lead), msgs, ai, &fakeSender{}, &fakeRetriever{}, &fakeSummaries{}, enq)

	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatal(err)
	}
	if ai.callCount() != 0 {
		t.Error("переотправка не должна звать Claude")
	}
	if len(enq.calls) != 1 {
		t.Errorf("постановок summary: %d, ожидали 1", len(enq.calls))
	}
}
