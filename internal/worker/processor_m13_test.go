// Контрактные тесты M13: Эмма уважает режим диалога (BUG-01, двойная
// проверка) и взводит контур напоминаний, когда молчит (LOGIC-01).
package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/settings"
)

// fakeSettings — settings.Reader без БД: значения из карты, иначе дефолт.
type fakeSettings map[string]int

func (f fakeSettings) Minutes(_ context.Context, key string) int {
	if v, ok := f[key]; ok {
		return v
	}
	return settings.Defaults[key]
}

// fakeTakeoverEnq — сборщик постановок takeover:* (сам Redis — контур
// интеграционных тестов).
type fakeTakeoverEnq struct {
	mu        sync.Mutex
	reminders []queue.TakeoverPayload
	pickups   []queue.TakeoverPayload
	delays    map[string][]time.Duration
	err       error
}

func newFakeTakeoverEnq() *fakeTakeoverEnq {
	return &fakeTakeoverEnq{delays: map[string][]time.Duration{}}
}

func (f *fakeTakeoverEnq) EnqueueTakeoverReminder(_ context.Context, p queue.TakeoverPayload, delay time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.reminders = append(f.reminders, p)
	f.delays[queue.TypeTakeoverReminder] = append(f.delays[queue.TypeTakeoverReminder], delay)
	return nil
}

func (f *fakeTakeoverEnq) EnqueueTakeoverPickup(_ context.Context, p queue.TakeoverPayload, delay time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.pickups = append(f.pickups, p)
	f.delays[queue.TypeTakeoverPickup] = append(f.delays[queue.TypeTakeoverPickup], delay)
	return nil
}

func (f *fakeTakeoverEnq) reminderCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reminders)
}

// recordingKanban — StateMachine, фиксирующий вызов OnInbound: Kanban-триггеры
// (§3.2/§3.5) обязаны отработать и при молчащей Эмме.
type recordingKanban struct {
	mu       sync.Mutex
	inbounds int
	silenced bool
}

func (k *recordingKanban) OnInbound(context.Context, *models.Lead) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.inbounds++
	return k.silenced, nil
}
func (k *recordingKanban) HandleTTLExpire(context.Context, int64) error { panic("не зовётся") }
func (k *recordingKanban) HandleTTLWarning(context.Context, queue.TTLWarnPayload) error {
	panic("не зовётся")
}
func (k *recordingKanban) HandleAntiSpamFollowup(context.Context, queue.AntiSpamPayload) error {
	panic("не зовётся")
}
func (k *recordingKanban) HandleAntiSpamEscalate(context.Context, queue.AntiSpamPayload) error {
	panic("не зовётся")
}

// m13Lead — свежий лид на каждый тест: фейки мутируют состояние (режим),
// общий testLead из M3 переиспользовать нельзя.
func m13Lead(mode string, silencedUntil *time.Time) *models.Lead {
	return &models.Lead{
		ID: 7, TelegramUserID: 424242, StageID: 2, MessageCount: 3,
		DialogMode: mode, BotSilencedUntil: silencedUntil,
	}
}

// newM13Processor — процессор с контуром takeover (Settings+TakeoverEnq)
// и Kanban-машиной.
func newM13Processor(t *testing.T, leads *fakeLeads, msgs *fakeMsgs, ai *fakeAI, snd *fakeSender, enq *fakeTakeoverEnq, kb StateMachine) *Processor {
	t.Helper()
	return NewProcessor(ProcessorDeps{
		Leads:       leads,
		Msgs:        msgs,
		Budgeter:    mustBudgeter(t),
		AI:          ai,
		Sender:      snd,
		Kanban:      kb,
		Settings:    fakeSettings{settings.KeyReminderMinutes: 10},
		TakeoverEnq: enq,
		Log:         testLogger(),
	})
}

func m13History(id int64, content string) []models.Message {
	return []models.Message{{
		ID: id, LeadID: 7, Direction: models.DirectionInbound,
		Content: content, CreatedAt: time.Now().Add(-time.Minute),
	}}
}

// TestM13_HumanMode_EmmaSilent — критерий 1: «Взять в работу» → inbound
// клиента НЕ порождает ответ Эммы; Kanban-триггеры отрабатывают; взводится
// напоминание с параметрами инициирующего inbound.
func TestM13_HumanMode_EmmaSilent(t *testing.T) {
	leads := newFakeLeads(m13Lead(models.DialogModeHuman, nil))
	msgs := &fakeMsgs{history: m13History(41, "мне нужна помощь")}
	ai := &fakeAI{reply: "не должно понадобиться"}
	snd := &fakeSender{}
	enq := newFakeTakeoverEnq()
	kb := &recordingKanban{}

	err := newM13Processor(t, leads, msgs, ai, snd, enq, kb).
		HandleProcessInbound(context.Background(), inboundTask(t, 7, 100))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if ai.callCount() != 0 {
		t.Errorf("claude вызван %d раз, ожидали 0 — диалог у менеджера", ai.callCount())
	}
	if len(msgs.created) != 0 || snd.sentCount() != 0 {
		t.Error("в режиме human не должно быть ни записи outbound, ни отправки")
	}
	if kb.inbounds != 1 {
		t.Errorf("kanban.OnInbound вызван %d раз, ожидали 1 (триггеры §3.2/§3.5 живут)", kb.inbounds)
	}
	if enq.reminderCount() != 1 {
		t.Fatalf("напоминаний взведено %d, ожидали 1", enq.reminderCount())
	}
	p := enq.reminders[0]
	if p.LeadID != 7 || p.MessageCount != 3 || p.InboundMsgID != 41 || p.TgMsgID != 100 {
		t.Errorf("payload напоминания: %+v", p)
	}
	if d := enq.delays[queue.TypeTakeoverReminder][0]; d != 10*time.Minute {
		t.Errorf("delay напоминания %v, ожидали 10м (settings)", d)
	}
}

// TestM13_ActivePause_EmmaSilent — пауза автопилота в будущем: Эмма молчит,
// напоминание взводится (клиент не должен повиснуть).
func TestM13_ActivePause_EmmaSilent(t *testing.T) {
	until := time.Now().Add(20 * time.Minute)
	leads := newFakeLeads(m13Lead(models.DialogModeBot, &until))
	msgs := &fakeMsgs{history: m13History(42, "ещё вопрос")}
	ai := &fakeAI{reply: "нет"}
	snd := &fakeSender{}
	enq := newFakeTakeoverEnq()

	err := newM13Processor(t, leads, msgs, ai, snd, enq, &recordingKanban{}).
		HandleProcessInbound(context.Background(), inboundTask(t, 7, 101))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if ai.callCount() != 0 || len(msgs.created) != 0 || snd.sentCount() != 0 {
		t.Error("при активной паузе Эмма обязана молчать")
	}
	if enq.reminderCount() != 1 {
		t.Errorf("напоминаний %d, ожидали 1", enq.reminderCount())
	}
}

// TestM13_ExpiredPause_EmmaAnswers — критерий 3 (вторая половина): пауза
// в прошлом = обычный режим bot, Эмма отвечает; напоминание не взводится.
func TestM13_ExpiredPause_EmmaAnswers(t *testing.T) {
	until := time.Now().Add(-time.Second)
	leads := newFakeLeads(m13Lead(models.DialogModeBot, &until))
	msgs := &fakeMsgs{history: m13History(43, "вы тут?")}
	ai := &fakeAI{reply: "конечно, я здесь"}
	snd := &fakeSender{}
	enq := newFakeTakeoverEnq()

	err := newM13Processor(t, leads, msgs, ai, snd, enq, &recordingKanban{}).
		HandleProcessInbound(context.Background(), inboundTask(t, 7, 102))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if ai.callCount() != 1 || len(msgs.created) != 1 || snd.sentCount() != 1 {
		t.Errorf("просроченная пауза: claude=%d created=%d sent=%d, ожидали 1/1/1",
			ai.callCount(), len(msgs.created), snd.sentCount())
	}
	if enq.reminderCount() != 0 {
		t.Errorf("напоминаний %d, ожидали 0 — Эмма ответила сама", enq.reminderCount())
	}
}

// TestM13_BUG01_ModeFlipsDuringGeneration — критерий 2 (гонка BUG-01):
// менеджер взял диалог МЕЖДУ вызовом Claude и CreateOutbound → ответ
// отбрасывается: в messages НЕТ строки, в Telegram ничего не ушло, задача
// завершена успешно (не ретрай); контур напоминаний взведён.
func TestM13_BUG01_ModeFlipsDuringGeneration(t *testing.T) {
	leads := newFakeLeads(m13Lead(models.DialogModeBot, nil))
	msgs := &fakeMsgs{history: m13History(44, "хочу оплатить")}
	snd := &fakeSender{}
	enq := newFakeTakeoverEnq()
	ai := &fakeAI{reply: "вот ссылка на оплату"}
	// Крючок «во время генерации»: смена режима из другой вкладки.
	ai.onComplete = func() { leads.setMode(7, models.DialogModeHuman, nil) }

	err := newM13Processor(t, leads, msgs, ai, snd, enq, &recordingKanban{}).
		HandleProcessInbound(context.Background(), inboundTask(t, 7, 103))
	if err != nil {
		t.Fatalf("handle обязан завершиться успешно (не ретрай): %v", err)
	}
	if ai.callCount() != 1 {
		t.Errorf("claude вызван %d раз, ожидали 1 (гонка началась после старта генерации)", ai.callCount())
	}
	if len(msgs.created) != 0 {
		t.Errorf("в messages %d строк — сгенерированный ответ обязан быть отброшен БЕЗ записи", len(msgs.created))
	}
	if snd.sentCount() != 0 {
		t.Error("в Telegram ничего не должно уйти — менеджер уже ведёт диалог")
	}
	if enq.reminderCount() != 1 {
		t.Errorf("напоминаний %d, ожидали 1 — inbound остался без ответа при менеджере", enq.reminderCount())
	}
}

// TestM13_BUG01_PauseFlipsDuringGeneration — та же гонка, но с паузой
// автопилота (менеджер ответил из карточки, пока Эмма думала).
func TestM13_BUG01_PauseFlipsDuringGeneration(t *testing.T) {
	leads := newFakeLeads(m13Lead(models.DialogModeBot, nil))
	msgs := &fakeMsgs{history: m13History(45, "вопрос")}
	snd := &fakeSender{}
	ai := &fakeAI{reply: "ответ Эммы"}
	until := time.Now().Add(30 * time.Minute)
	ai.onComplete = func() {
		leads.setMode(7, models.DialogModeBot, &until)
		// Реплика менеджера уже в истории — напоминание не взводится.
		author := models.AuthorManagerPrefix + "1"
		msgs.history = append(msgs.history, models.Message{
			ID: 46, LeadID: 7, Direction: models.DirectionOutbound,
			Author: &author, Content: "я отвечу сам",
		})
	}
	enq := newFakeTakeoverEnq()

	err := newM13Processor(t, leads, msgs, ai, snd, enq, &recordingKanban{}).
		HandleProcessInbound(context.Background(), inboundTask(t, 7, 104))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(msgs.created) != 0 || snd.sentCount() != 0 {
		t.Error("ответ Эммы обязан быть отброшен: менеджер уже ответил, идёт пауза")
	}
	if enq.reminderCount() != 0 {
		t.Errorf("напоминаний %d, ожидали 0 — последняя реплика уже от менеджера", enq.reminderCount())
	}
}

// TestM13_NonText_RespectsMode — нетекстовое входящее при human: подсказка
// nonTextReply тоже не отправляется (все точки отправки уважают режим).
func TestM13_NonText_RespectsMode(t *testing.T) {
	leads := newFakeLeads(m13Lead(models.DialogModeHuman, nil))
	msgs := &fakeMsgs{history: []models.Message{{
		ID: 47, LeadID: 7, Direction: models.DirectionInbound, Content: "",
		CreatedAt: time.Now(),
	}}}
	snd := &fakeSender{}
	enq := newFakeTakeoverEnq()

	err := newM13Processor(t, leads, msgs, &fakeAI{}, snd, enq, &recordingKanban{}).
		HandleProcessInbound(context.Background(), inboundTask(t, 7, 105))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(msgs.created) != 0 || snd.sentCount() != 0 {
		t.Error("подсказка о нетекстовом не должна перебивать менеджера")
	}
	if enq.reminderCount() != 1 {
		t.Errorf("напоминаний %d, ожидали 1", enq.reminderCount())
	}
}

// TestM13_DuplicateReminder — дубль постановки (ErrDuplicate) не роняет
// задачу: §4.5 в работе.
func TestM13_DuplicateReminder(t *testing.T) {
	leads := newFakeLeads(m13Lead(models.DialogModeHuman, nil))
	msgs := &fakeMsgs{history: m13History(48, "повторный апдейт")}
	enq := newFakeTakeoverEnq()
	enq.err = queue.ErrDuplicate

	err := newM13Processor(t, leads, msgs, &fakeAI{}, &fakeSender{}, enq, &recordingKanban{}).
		HandleProcessInbound(context.Background(), inboundTask(t, 7, 106))
	if err != nil {
		t.Fatalf("ErrDuplicate постановки не должен быть ошибкой задачи: %v", err)
	}
}
