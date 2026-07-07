package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/claude"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// --- фейки зависимостей ---

type fakeLeads struct {
	mu    sync.Mutex
	leads map[int64]*models.Lead
}

func newFakeLeads(leads ...*models.Lead) *fakeLeads {
	f := &fakeLeads{leads: map[int64]*models.Lead{}}
	for _, l := range leads {
		f.leads[l.ID] = l
	}
	return f
}

func (f *fakeLeads) Create(_ context.Context, lead *models.Lead) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leads[lead.ID] = lead
	return nil
}

func (f *fakeLeads) GetByID(_ context.Context, id int64) (*models.Lead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lead, ok := f.leads[id]
	if !ok {
		return nil, repo.ErrNotFound
	}
	cp := *lead
	return &cp, nil
}

func (f *fakeLeads) GetByTelegramUserID(_ context.Context, tgID int64) (*models.Lead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.leads {
		if l.TelegramUserID == tgID {
			cp := *l
			return &cp, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (f *fakeLeads) List(context.Context, repo.ListLeadsParams) ([]models.Lead, int64, error) {
	panic("воркер не листает лидов (метод M8)")
}

func (f *fakeLeads) Save(_ context.Context, lead *models.Lead) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leads[lead.ID] = lead
	return nil
}

func (f *fakeLeads) UpdateFields(_ context.Context, id int64, fields map[string]interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	lead, ok := f.leads[id]
	if !ok {
		return repo.ErrNotFound
	}
	if v, ok := fields["ttl_task_id"]; ok {
		if v == nil {
			lead.TTLTaskID = nil
		} else {
			s := v.(string)
			lead.TTLTaskID = &s
		}
	}
	if v, ok := fields["escalated_at"]; ok { // M5: antispam:escalate
		ts := v.(time.Time)
		lead.EscalatedAt = &ts
	}
	if v, ok := fields["last_activity_at"]; ok { // M5: ResetTTL (IQ-4)
		lead.LastActivityAt = v.(time.Time)
	}
	return nil
}

func (f *fakeLeads) TransitionStage(_ context.Context, id int64, from, to int16) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lead, ok := f.leads[id]
	if !ok || lead.StageID != from {
		return false, nil
	}
	lead.StageID = to
	lead.AntiSpamCount = 0
	lead.LastActivityAt = time.Now() // точка отсчёта TTL (CLAUDE.md §4.7)
	return true, nil
}

type fakeMsgs struct {
	mu      sync.Mutex
	history []models.Message
	outErr  error
	created []models.Message // сохранённые outbound
}

func (f *fakeMsgs) CreateInbound(_ context.Context, m *models.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m.Direction = models.DirectionInbound
	f.history = append(f.history, *m)
	return nil
}

func (f *fakeMsgs) CreateOutbound(_ context.Context, m *models.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outErr != nil {
		return f.outErr
	}
	m.Direction = models.DirectionOutbound
	f.created = append(f.created, *m)
	f.history = append(f.history, *m)
	return nil
}

func (f *fakeMsgs) ListByLeadBefore(ctx context.Context, leadID, _ int64, limit int) ([]models.Message, error) {
	return f.ListByLead(ctx, leadID, limit)
}

func (f *fakeMsgs) ListByLead(_ context.Context, leadID int64, limit int) ([]models.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]models.Message, 0, len(f.history))
	for _, m := range f.history {
		if m.LeadID == leadID {
			out = append(out, m)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

type sent struct {
	chatID int64
	text   string
}

type fakeSender struct {
	mu      sync.Mutex
	typing  []int64
	sent    []sent
	sendErr error // ошибка ПЕРВОГО Send (потом сбрасывается) — сценарий ретрая
}

func (f *fakeSender) Typing(chatID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.typing = append(f.typing, chatID)
	return nil
}

func (f *fakeSender) Send(chatID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		err := f.sendErr
		f.sendErr = nil
		return err
	}
	f.sent = append(f.sent, sent{chatID: chatID, text: text})
	return nil
}

func (f *fakeSender) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type fakeAI struct {
	mu      sync.Mutex
	calls   int
	reply   string
	err     error
	systems []string // system-промпты входящих вызовов (проверка RAG/summary M4)
}

func (f *fakeAI) Complete(_ context.Context, system string, _ []claude.Message) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.systems = append(f.systems, system)
	if f.err != nil {
		return "", f.err
	}
	return f.reply, nil
}

func (f *fakeAI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeAI) lastSystem() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.systems) == 0 {
		return ""
	}
	return f.systems[len(f.systems)-1]
}

// --- сборка ---

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError + 4}))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func inboundTask(t *testing.T, leadID int64, msgID int) *asynq.Task {
	t.Helper()
	payload, err := json.Marshal(queue.InboundPayload{LeadID: leadID, MsgID: msgID})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return asynq.NewTask(queue.TypeProcessInbound, payload)
}

func newTestProcessor(t *testing.T, leads *fakeLeads, msgs *fakeMsgs, ai *fakeAI, snd *fakeSender) *Processor {
	t.Helper()
	// Retriever/Summaries/SummaryEnq не заданы — сценарии M3 (диалог без RAG);
	// сценарии M4 собирают процессор сами (processor_rag_test.go).
	return NewProcessor(ProcessorDeps{
		Leads:    leads,
		Msgs:     msgs,
		Budgeter: mustBudgeter(t),
		AI:       ai,
		Sender:   snd,
		Log:      testLogger(),
	})
}

var testLead = &models.Lead{ID: 7, TelegramUserID: 424242, StageID: 1}

// --- тесты ---

// TestHandle_HappyPath — пайплайн §6.2: typing → Claude → save outbound → send.
func TestHandle_HappyPath(t *testing.T) {
	leads := newFakeLeads(testLead)
	msgs := &fakeMsgs{history: []models.Message{
		{LeadID: 7, Direction: models.DirectionInbound, Content: "Здравствуйте!"},
	}}
	ai := &fakeAI{reply: "Добрый день! Чем могу помочь?"}
	snd := &fakeSender{}

	err := newTestProcessor(t, leads, msgs, ai, snd).
		HandleProcessInbound(context.Background(), inboundTask(t, 7, 100))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(snd.typing) != 1 || snd.typing[0] != 424242 {
		t.Errorf("typing: %v, ожидали [424242] (§6.2 шаг 1)", snd.typing)
	}
	if ai.callCount() != 1 {
		t.Errorf("claude вызван %d раз, ожидали 1", ai.callCount())
	}
	if len(msgs.created) != 1 {
		t.Fatalf("outbound сохранён %d раз, ожидали 1", len(msgs.created))
	}
	out := msgs.created[0]
	if out.Direction != models.DirectionOutbound || out.Content != ai.reply || out.LeadID != 7 {
		t.Errorf("сохранённый outbound: %+v", out)
	}
	if out.Tokens == nil || *out.Tokens != estimateTokens(ai.reply) {
		t.Errorf("outbound.Tokens = %v, ожидали оценку ответа", out.Tokens)
	}
	if len(snd.sent) != 1 || snd.sent[0] != (sent{chatID: 424242, text: ai.reply}) {
		t.Errorf("send: %+v", snd.sent)
	}
}

// TestHandle_LeadErased — лид стёрт (LGPD) → задача пропускается без ретрая.
func TestHandle_LeadErased(t *testing.T) {
	leads := newFakeLeads() // пусто
	msgs := &fakeMsgs{}
	ai := &fakeAI{reply: "x"}
	snd := &fakeSender{}

	err := newTestProcessor(t, leads, msgs, ai, snd).
		HandleProcessInbound(context.Background(), inboundTask(t, 7, 100))
	if err != nil {
		t.Fatalf("ожидали nil (без ретрая), получили: %v", err)
	}
	if ai.callCount() != 0 || snd.sentCount() != 0 {
		t.Error("для стёртого лида не должно быть ни Claude, ни отправок")
	}
}

// TestHandle_MalformedPayload — битый payload → SkipRetry (сразу в архив).
func TestHandle_MalformedPayload(t *testing.T) {
	p := newTestProcessor(t, newFakeLeads(testLead), &fakeMsgs{}, &fakeAI{}, &fakeSender{})
	err := p.HandleProcessInbound(context.Background(),
		asynq.NewTask(queue.TypeProcessInbound, []byte("не json")))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("ожидали SkipRetry, получили: %v", err)
	}
}

// TestHandle_ClaudeDown — ошибка Claude → ошибка задачи (ретраи по §6.3),
// ничего не сохранено и не отправлено.
func TestHandle_ClaudeDown(t *testing.T) {
	msgs := &fakeMsgs{history: []models.Message{
		{LeadID: 7, Direction: models.DirectionInbound, Content: "привет"},
	}}
	ai := &fakeAI{err: errors.New("api 529: overloaded_error")}
	snd := &fakeSender{}

	err := newTestProcessor(t, newFakeLeads(testLead), msgs, ai, snd).
		HandleProcessInbound(context.Background(), inboundTask(t, 7, 100))
	if err == nil {
		t.Fatal("падение Claude обязано возвращать ошибку (ретрай Asynq)")
	}
	if len(msgs.created) != 0 || snd.sentCount() != 0 {
		t.Error("при ошибке Claude не должно быть ни записи, ни отправки")
	}
}

// TestHandle_RetryAfterSendFailure — идемпотентность ретрая: ответ сохранён,
// Send упал; повторная попытка переотправляет БЕЗ второго вызова Claude
// и без второй строки в messages.
func TestHandle_RetryAfterSendFailure(t *testing.T) {
	msgs := &fakeMsgs{history: []models.Message{
		{LeadID: 7, Direction: models.DirectionInbound, Content: "привет"},
	}}
	ai := &fakeAI{reply: "ответ бота"}
	snd := &fakeSender{sendErr: errors.New("telegram: 502")}
	p := newTestProcessor(t, newFakeLeads(testLead), msgs, ai, snd)

	task := inboundTask(t, 7, 100)

	// Попытка 1: Claude отработал, outbound сохранён, Send упал → ошибка.
	if err := p.HandleProcessInbound(context.Background(), task); err == nil {
		t.Fatal("первая попытка обязана вернуть ошибку Send")
	}
	if len(msgs.created) != 1 {
		t.Fatalf("ответ должен быть сохранён до Send (§6.2), created = %d", len(msgs.created))
	}

	// Попытка 2 (ретрай Asynq): последним в истории лежит outbound.
	if err := p.HandleProcessInbound(context.Background(), task); err != nil {
		t.Fatalf("ретрай: %v", err)
	}
	if ai.callCount() != 1 {
		t.Errorf("claude вызван %d раз, ожидали 1 (идемпотентность ретрая)", ai.callCount())
	}
	if len(msgs.created) != 1 {
		t.Errorf("в messages %d outbound-строк, ожидали 1", len(msgs.created))
	}
	if snd.sentCount() != 1 || snd.sent[0].text != "ответ бота" {
		t.Errorf("переотправка: %+v", snd.sent)
	}
}

// TestHandle_DuplicateTaskAfterReply — дубль задачи после успешного ответа:
// повторная доставка ничего не ломает (второй ответ не генерируется).
func TestHandle_DuplicateTaskAfterReply(t *testing.T) {
	msgs := &fakeMsgs{history: []models.Message{
		{LeadID: 7, Direction: models.DirectionInbound, Content: "привет"},
	}}
	ai := &fakeAI{reply: "ответ"}
	snd := &fakeSender{}
	p := newTestProcessor(t, newFakeLeads(testLead), msgs, ai, snd)
	task := inboundTask(t, 7, 100)

	if err := p.HandleProcessInbound(context.Background(), task); err != nil {
		t.Fatalf("первая обработка: %v", err)
	}
	if err := p.HandleProcessInbound(context.Background(), task); err != nil {
		t.Fatalf("дубль: %v", err)
	}
	if ai.callCount() != 1 {
		t.Errorf("claude вызван %d раз — дубль не должен генерировать второй ответ", ai.callCount())
	}
	if len(msgs.created) != 1 {
		t.Errorf("outbound-строк: %d, ожидали 1", len(msgs.created))
	}
}

// TestHandle_ErrorsAreWrapped — ошибки несут контекст (§5, fmt.Errorf %w).
func TestHandle_ErrorsAreWrapped(t *testing.T) {
	msgs := &fakeMsgs{history: []models.Message{
		{LeadID: 7, Direction: models.DirectionInbound, Content: "привет"},
	}}
	base := errors.New("insert failed")
	msgs.outErr = fmt.Errorf("repo: create outbound message: %w", base)
	p := newTestProcessor(t, newFakeLeads(testLead), msgs, &fakeAI{reply: "r"}, &fakeSender{})

	err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100))
	if err == nil || !errors.Is(err, base) {
		t.Errorf("ошибка репозитория должна прокидываться через %%w, получили: %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "сохранение ответа") {
		t.Errorf("ошибка без контекста шага: %v", err)
	}
}

// Заглушки M11 (recovery-cron pending_task работает с боевым leadRepo,
// в этих тестах не участвует).
func (f *fakeLeads) ListPendingTask(context.Context, int) ([]models.Lead, error) {
	panic("pending_task здесь не используется (M11)")
}

func (f *fakeLeads) ClearPendingTask(context.Context, int64, int) (bool, error) {
	panic("pending_task здесь не используется (M11)")
}

// TestHandle_NonTextInbound — голос/стикер/фото: content пуст, диалог для
// Claude кончается репликой ассистента (или пуст) → Claude НЕ вызывается,
// лид получает детерминированную подсказку (prefill-400 Sonnet 5, прод-баг).
func TestHandle_NonTextInbound(t *testing.T) {
	cases := map[string][]models.Message{
		"голос посреди диалога": {
			{LeadID: 7, Direction: models.DirectionInbound, Content: "Здравствуйте!"},
			{LeadID: 7, Direction: models.DirectionOutbound, Content: "Добрый день!"},
			{LeadID: 7, Direction: models.DirectionInbound, Content: ""},
		},
		"первое сообщение — голос": {
			{LeadID: 7, Direction: models.DirectionInbound, Content: "   "},
		},
	}
	for name, history := range cases {
		t.Run(name, func(t *testing.T) {
			leads := newFakeLeads(testLead)
			msgs := &fakeMsgs{history: history}
			ai := &fakeAI{reply: "не должно понадобиться"}
			snd := &fakeSender{}

			err := newTestProcessor(t, leads, msgs, ai, snd).
				HandleProcessInbound(context.Background(), inboundTask(t, 7, 100))
			if err != nil {
				t.Fatalf("handle: %v", err)
			}
			if ai.callCount() != 0 {
				t.Errorf("claude вызван %d раз, ожидали 0", ai.callCount())
			}
			if len(msgs.created) != 1 || msgs.created[0].Content != nonTextReply {
				t.Fatalf("outbound: %+v, ожидали одну подсказку nonTextReply", msgs.created)
			}
			if len(snd.sent) != 1 || snd.sent[0].text != nonTextReply {
				t.Errorf("send: %+v, ожидали подсказку лиду", snd.sent)
			}
		})
	}
}
