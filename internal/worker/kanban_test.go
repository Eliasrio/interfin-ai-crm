// Тесты интеграции state machine в пайплайн воркера (M5):
// молчание бота при anti-spam (IQ-9) и маршрутизация отложенных задач
// в kanban.Machine. Бизнес-логика самой машины — internal/kanban.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
)

// fakeMachine — StateMachine, программируемый на silenced/ошибку.
type fakeMachine struct {
	mu        sync.Mutex
	silenced  bool
	err       error
	onInbound int
	ttlExpire []int64
	followups []queue.AntiSpamPayload
	escalates []queue.AntiSpamPayload
}

func (f *fakeMachine) OnInbound(_ context.Context, _ *models.Lead) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onInbound++
	return f.silenced, f.err
}

func (f *fakeMachine) HandleTTLExpire(_ context.Context, leadID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ttlExpire = append(f.ttlExpire, leadID)
	return f.err
}

func (f *fakeMachine) HandleAntiSpamFollowup(_ context.Context, p queue.AntiSpamPayload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.followups = append(f.followups, p)
	return f.err
}

func (f *fakeMachine) HandleAntiSpamEscalate(_ context.Context, p queue.AntiSpamPayload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.escalates = append(f.escalates, p)
	return f.err
}

func newKanbanProcessor(t *testing.T, m StateMachine, leads *fakeLeads, msgs *fakeMsgs, ai *fakeAI, snd *fakeSender) *Processor {
	t.Helper()
	return NewProcessor(ProcessorDeps{
		Leads:    leads,
		Msgs:     msgs,
		Budgeter: mustBudgeter(t),
		AI:       ai,
		Sender:   snd,
		Kanban:   m,
		Log:      testLogger(),
	})
}

// TestHandle_SilencedLeadGetsNoReply — IQ-9: лид за anti-spam лимитом не
// получает ответа — ни вызова Claude, ни typing, ни outbound.
func TestHandle_SilencedLeadGetsNoReply(t *testing.T) {
	leads := newFakeLeads(&models.Lead{ID: 7, TelegramUserID: 424242, StageID: 2, AntiSpamCount: 25})
	msgs := &fakeMsgs{history: []models.Message{
		{LeadID: 7, Direction: models.DirectionInbound, Content: "ало?????"},
	}}
	ai := &fakeAI{reply: "не должно отправиться"}
	snd := &fakeSender{}
	machine := &fakeMachine{silenced: true}

	err := newKanbanProcessor(t, machine, leads, msgs, ai, snd).
		HandleProcessInbound(context.Background(), inboundTask(t, 7, 100))
	if err != nil {
		t.Fatalf("молчание — штатный исход, не ошибка: %v", err)
	}
	if machine.onInbound != 1 {
		t.Errorf("OnInbound вызван %d раз, ожидали 1", machine.onInbound)
	}
	if ai.callCount() != 0 {
		t.Errorf("Claude вызван для замолчанного лида (%d раз) — трата токенов", ai.callCount())
	}
	if len(snd.sent) != 0 || len(snd.typing) != 0 {
		t.Errorf("замолчанному лиду что-то отправлено: sent=%v typing=%v", snd.sent, snd.typing)
	}
	if len(msgs.created) != 0 {
		t.Errorf("создан outbound при молчании: %v", msgs.created)
	}
}

// TestHandle_KanbanErrorRetriesTask — ошибка state machine (Redis умер при
// взводе задач) → ошибка задачи → ретрай Asynq; шаги машины идемпотентны.
func TestHandle_KanbanErrorRetriesTask(t *testing.T) {
	leads := newFakeLeads(&models.Lead{ID: 7, TelegramUserID: 424242, StageID: 1})
	msgs := &fakeMsgs{history: []models.Message{
		{LeadID: 7, Direction: models.DirectionInbound, Content: "привет"},
	}}
	ai := &fakeAI{reply: "r"}
	machine := &fakeMachine{err: errors.New("redis down")}

	err := newKanbanProcessor(t, machine, leads, msgs, ai, &fakeSender{}).
		HandleProcessInbound(context.Background(), inboundTask(t, 7, 100))
	if err == nil {
		t.Fatal("ожидали ошибку для ретрая")
	}
	if ai.callCount() != 0 {
		t.Error("Claude вызван до успешного прохода state machine")
	}
}

// TestHandle_NilKanbanKeepsM3Behaviour — без машины (юнит-режим M3)
// пайплайн работает как раньше.
func TestHandle_NilKanbanKeepsM3Behaviour(t *testing.T) {
	leads := newFakeLeads(&models.Lead{ID: 7, TelegramUserID: 424242, StageID: 1})
	msgs := &fakeMsgs{history: []models.Message{
		{LeadID: 7, Direction: models.DirectionInbound, Content: "привет"},
	}}
	ai := &fakeAI{reply: "ответ"}
	snd := &fakeSender{}

	err := newTestProcessor(t, leads, msgs, ai, snd).
		HandleProcessInbound(context.Background(), inboundTask(t, 7, 100))
	if err != nil {
		t.Fatal(err)
	}
	if len(snd.sent) != 1 {
		t.Errorf("ответ не отправлен: %v", snd.sent)
	}
}

// --- маршрутизация отложенных задач ---

func TestKanbanHandlers_RouteToMachine(t *testing.T) {
	machine := &fakeMachine{}
	h := NewKanbanHandlers(machine, testLogger())
	ctx := context.Background()

	ttlPayload, _ := json.Marshal(queue.TTLPayload{LeadID: 42})
	if err := h.HandleTTLExpire(ctx, asynq.NewTask(queue.TypeTTLExpire, ttlPayload)); err != nil {
		t.Fatal(err)
	}
	if len(machine.ttlExpire) != 1 || machine.ttlExpire[0] != 42 {
		t.Errorf("ttl:expire не дошёл до машины: %v", machine.ttlExpire)
	}

	asPayload, _ := json.Marshal(queue.AntiSpamPayload{LeadID: 42, StageID: 2})
	if err := h.HandleAntiSpamFollowup(ctx, asynq.NewTask(queue.TypeAntiSpamFollowup, asPayload)); err != nil {
		t.Fatal(err)
	}
	if err := h.HandleAntiSpamEscalate(ctx, asynq.NewTask(queue.TypeAntiSpamEscalate, asPayload)); err != nil {
		t.Fatal(err)
	}
	want := queue.AntiSpamPayload{LeadID: 42, StageID: 2}
	if len(machine.followups) != 1 || machine.followups[0] != want {
		t.Errorf("followup payload: %v", machine.followups)
	}
	if len(machine.escalates) != 1 || machine.escalates[0] != want {
		t.Errorf("escalate payload: %v", machine.escalates)
	}
}

func TestKanbanHandlers_MalformedPayloadSkipsRetry(t *testing.T) {
	h := NewKanbanHandlers(&fakeMachine{}, testLogger())
	ctx := context.Background()
	for _, task := range []*asynq.Task{
		asynq.NewTask(queue.TypeTTLExpire, []byte("мусор")),
		asynq.NewTask(queue.TypeAntiSpamFollowup, []byte("{")),
		asynq.NewTask(queue.TypeAntiSpamEscalate, []byte("")),
	} {
		err := h.HandleTTLExpire(ctx, task)
		switch task.Type() {
		case queue.TypeAntiSpamFollowup:
			err = h.HandleAntiSpamFollowup(ctx, task)
		case queue.TypeAntiSpamEscalate:
			err = h.HandleAntiSpamEscalate(ctx, task)
		}
		if !errors.Is(err, asynq.SkipRetry) {
			t.Errorf("%s: битый payload обязан дать SkipRetry (§6.3), получили %v", task.Type(), err)
		}
	}
}
