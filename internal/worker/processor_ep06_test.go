// Contract-тесты EP-06 (вкладка 6): reply-событие с usage и временем
// ответа — единственный источник учёта расходов Claude (фронт EP-07 и
// stats читают эти поля), события ошибок llm_api/timeout/telegram_api,
// ветка переотправки без второго reply, сигналы контуру алертов.
package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/interfin/interfin-ai-crm/internal/claude"
	"github.com/interfin/interfin-ai-crm/internal/models"
)

// fakeAlerts — AlertSink: считает OnError/OnSuccess (проверка recordEvent-точек).
type fakeAlerts struct {
	mu        sync.Mutex
	errKinds  []string
	errDetail []string
	successes int
}

func (f *fakeAlerts) OnError(_ context.Context, kind, detail string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errKinds = append(f.errKinds, kind)
	f.errDetail = append(f.errDetail, detail)
}

func (f *fakeAlerts) OnSuccess(context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.successes++
}

func (f *fakeAlerts) snapshot() ([]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.errKinds...), f.successes
}

// newEP06Processor — минимальный процессор с журналом событий и алертами.
func newEP06Processor(t *testing.T, ai *fakeAI, snd *fakeSender) (*Processor, *fakeMsgs, *fakeEvents, *fakeAlerts) {
	t.Helper()
	msgs := &fakeMsgs{history: []models.Message{
		{LeadID: 7, Direction: models.DirectionInbound, Content: "Здравствуйте, расскажите об услугах"},
	}}
	events := &fakeEvents{}
	alerts := &fakeAlerts{}
	lead := *testLead
	p := NewProcessor(ProcessorDeps{
		Leads:    newFakeLeads(&lead),
		Msgs:     msgs,
		Budgeter: mustBudgeter(t),
		AI:       ai,
		Sender:   snd,
		Events:   events,
		Alerts:   alerts,
		Log:      testLogger(),
	})
	return p, msgs, events, alerts
}

// Критерий приёмки: успешный ответ → РОВНО ОДИН reply-event с tokens_in/out
// из usage и response_time_ms > 0; контур алертов получил OnSuccess.
func TestEP06_ReplyEventWithUsage(t *testing.T) {
	ai := &fakeAI{reply: "Добрый день!", usage: claude.Usage{InputTokens: 321, OutputTokens: 45}}
	p, _, events, alerts := newEP06Processor(t, ai, &fakeSender{})

	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	replies := events.byType(models.EmmaEventReply)
	if len(replies) != 1 {
		t.Fatalf("reply-событий %d, ждали ровно 1: %+v", len(replies), events.events)
	}
	ev := replies[0]
	if ev.TokensIn == nil || *ev.TokensIn != 321 || ev.TokensOut == nil || *ev.TokensOut != 45 {
		t.Errorf("токены: in=%v out=%v, ждали 321/45 из usage", ev.TokensIn, ev.TokensOut)
	}
	if ev.ResponseTimeMs == nil || *ev.ResponseTimeMs <= 0 {
		t.Errorf("response_time_ms = %v, ждали > 0", ev.ResponseTimeMs)
	}
	if ev.LeadID == nil || *ev.LeadID != 7 {
		t.Errorf("lead_id = %v, ждали 7", ev.LeadID)
	}
	if errs := events.byType(models.EmmaEventError); len(errs) != 0 {
		t.Errorf("ошибок не ждали: %+v", errs)
	}
	if kinds, successes := alerts.snapshot(); len(kinds) != 0 || successes != 1 {
		t.Errorf("алерты: kinds=%v successes=%d, ждали OnSuccess ровно раз", kinds, successes)
	}
}

// Критерий приёмки: упавший Send → error/telegram_api БЕЗ reply-события;
// ретрай уходит в ветку переотправки — второй вызов Claude и второй reply
// НЕ появляются.
func TestEP06_FailedSendThenResendNoReplyEvent(t *testing.T) {
	ai := &fakeAI{reply: "Ответ", usage: claude.Usage{InputTokens: 10, OutputTokens: 5}}
	snd := &fakeSender{sendErr: errors.New("telegram: send: 502 Bad Gateway")}
	p, _, events, _ := newEP06Processor(t, ai, snd)
	ctx := context.Background()

	// Попытка 1: save прошёл, Send упал → задача на ретрай.
	if err := p.HandleProcessInbound(ctx, inboundTask(t, 7, 100)); err == nil {
		t.Fatal("ждали ошибку упавшего Send")
	}
	if got := events.byType(models.EmmaEventReply); len(got) != 0 {
		t.Fatalf("reply при упавшем Send не пишется: %+v", got)
	}
	errs := events.byType(models.EmmaEventError)
	if len(errs) != 1 || errs[0].ErrorKind == nil || *errs[0].ErrorKind != models.EmmaErrTelegramAPI {
		t.Fatalf("ждали одну ошибку telegram_api: %+v", errs)
	}

	// Попытка 2 (ретрай Asynq): последний в истории outbound → переотправка.
	if err := p.HandleProcessInbound(ctx, inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("ретрай: %v", err)
	}
	if ai.callCount() != 1 {
		t.Errorf("claude вызван %d раз, ретрай не должен звать его повторно", ai.callCount())
	}
	if got := events.byType(models.EmmaEventReply); len(got) != 0 {
		t.Errorf("ветка переотправки НЕ пишет reply-событий (критерий EP-06): %+v", got)
	}
}

// Критерий приёмки: ошибка Claude → error/llm_api; detail краток и не
// содержит текста промпта/диалога.
func TestEP06_ClaudeErrorEvent(t *testing.T) {
	ai := &fakeAI{err: errors.New("claude: complete: api 529 Overloaded: overloaded_error: Overloaded")}
	p, _, events, alerts := newEP06Processor(t, ai, &fakeSender{})

	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err == nil {
		t.Fatal("ждали ошибку вызова claude (ретрай Asynq)")
	}

	errs := events.byType(models.EmmaEventError)
	if len(errs) != 1 || errs[0].ErrorKind == nil || *errs[0].ErrorKind != models.EmmaErrLLMAPI {
		t.Fatalf("ждали одну ошибку llm_api: %+v", errs)
	}
	detail := *errs[0].Detail
	if !strings.Contains(detail, "api 529") {
		t.Errorf("detail без сути ошибки API: %q", detail)
	}
	if strings.Contains(detail, "Здравствуйте") || strings.Contains(detail, "услугах") {
		t.Errorf("detail содержит текст диалога (запрещено): %q", detail)
	}
	if kinds, successes := alerts.snapshot(); successes != 0 || len(kinds) != 1 || kinds[0] != models.EmmaErrLLMAPI {
		t.Errorf("алерты: kinds=%v successes=%d, ждали один OnError(llm_api)", kinds, successes)
	}
	if got := events.byType(models.EmmaEventReply); len(got) != 0 {
		t.Errorf("reply при ошибке Claude не пишется: %+v", got)
	}
}

// Критерий приёмки: таймаут (контекст истёк) → error/timeout, не llm_api;
// различение по errors.Is.
func TestEP06_TimeoutEvent(t *testing.T) {
	ai := &fakeAI{err: fmt.Errorf("claude: complete: do request: %w", context.DeadlineExceeded)}
	p, _, events, alerts := newEP06Processor(t, ai, &fakeSender{})

	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err == nil {
		t.Fatal("ждали ошибку таймаута")
	}
	errs := events.byType(models.EmmaEventError)
	if len(errs) != 1 || errs[0].ErrorKind == nil || *errs[0].ErrorKind != models.EmmaErrTimeout {
		t.Fatalf("ждали одну ошибку timeout: %+v", errs)
	}
	// timeout не входит в условия алертов (ТЗ §3) — но OnError уходит,
	// Notifier сам игнорирует нерелевантные виды. Проверяем сам сигнал.
	if kinds, _ := alerts.snapshot(); len(kinds) != 1 || kinds[0] != models.EmmaErrTimeout {
		t.Errorf("алерты получили %v, ждали [timeout]", kinds)
	}
}

// Осознанное решение task §1: детерминированные ответы панели (welcome)
// reply-событий НЕ пишут — Claude не вызывался, токенов нет.
func TestEP06_WelcomeWritesNoReplyEvent(t *testing.T) {
	ai := &fakeAI{reply: "не должен вызываться"}
	rig := newEP05Rig(t, ep05Lead(models.DialogModeBot), "/start", ep05Scenario(), ai)

	if err := rig.p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if ai.callCount() != 0 {
		t.Fatalf("welcome не должен звать Claude, вызовов: %d", ai.callCount())
	}
	if got := rig.events.byType(models.EmmaEventReply); len(got) != 0 {
		t.Errorf("welcome не пишет reply-событий: %+v", got)
	}
}
