// Контрактные тесты обработчиков takeover:reminder / takeover:pickup (M13,
// LOGIC-01): уведомление менеджеру, подхват Эммой, самогашение устаревших
// задач. Цепочка вызывается напрямую, без реального ожидания минут
// (task M13 §«Как прогнать проверки»).
package worker

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/settings"
)

const testManagerChat = int64(90001)

// fakeInboundEnq — сборщик enqueue process:inbound (подхват Эммы).
type fakeInboundEnq struct {
	mu    sync.Mutex
	calls []struct {
		LeadID int64
		MsgID  int
	}
	err error
}

func (f *fakeInboundEnq) EnqueueInbound(_ context.Context, leadID int64, msgID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, struct {
		LeadID int64
		MsgID  int
	}{leadID, msgID})
	return nil
}

type takeoverRig struct {
	handlers *TakeoverHandlers
	leads    *fakeLeads
	msgs     *fakeMsgs
	snd      *fakeSender
	pub      *fakePublisher
	enq      *fakeTakeoverEnq
	inbound  *fakeInboundEnq
}

func newTakeoverRig(lead *models.Lead, history []models.Message) *takeoverRig {
	rig := &takeoverRig{
		leads:   newFakeLeads(lead),
		msgs:    &fakeMsgs{history: history},
		snd:     &fakeSender{},
		pub:     &fakePublisher{},
		enq:     newFakeTakeoverEnq(),
		inbound: &fakeInboundEnq{},
	}
	rig.handlers = NewTakeoverHandlers(TakeoverDeps{
		Leads:         rig.leads,
		Msgs:          rig.msgs,
		Settings:      fakeSettings{settings.KeyPickupMinutes: 10},
		Enq:           rig.enq,
		InboundEnq:    rig.inbound,
		Sender:        rig.snd,
		ManagerChatID: testManagerChat,
		Pub:           rig.pub,
		Log:           testLogger(),
	})
	return rig
}

func takeoverTask(t *testing.T, typ string, p queue.TakeoverPayload) *asynq.Task {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return asynq.NewTask(typ, raw)
}

func testPayload() queue.TakeoverPayload {
	return queue.TakeoverPayload{
		LeadID: 7, MessageCount: 3, InboundMsgID: 41,
		InboundAt: time.Now().Add(-10 * time.Minute), TgMsgID: 100,
	}
}

// TestTakeoverReminder_Fires — критерий 4 (первая половина): клиент ждёт в
// режиме human → менеджеру уходит Telegram-уведомление + WS-событие
// takeover_reminder + взводится подхват через pickup_minutes.
func TestTakeoverReminder_Fires(t *testing.T) {
	rig := newTakeoverRig(m13Lead(models.DialogModeHuman, nil), m13History(41, "жду ответа"))

	err := rig.handlers.HandleReminder(context.Background(),
		takeoverTask(t, queue.TypeTakeoverReminder, testPayload()))
	if err != nil {
		t.Fatalf("reminder: %v", err)
	}
	if len(rig.snd.sent) != 1 || rig.snd.sent[0].chatID != testManagerChat {
		t.Fatalf("уведомление менеджеру: %+v", rig.snd.sent)
	}
	if !strings.Contains(rig.snd.sent[0].text, "#7") ||
		!strings.Contains(rig.snd.sent[0].text, "10 мин") {
		t.Errorf("текст уведомления без лида/минут: %q", rig.snd.sent[0].text)
	}
	evs := rig.pub.byTypeM13(events.TypeTakeoverReminder)
	if len(evs) != 1 || evs[0].LeadID != 7 || evs[0].WaitingMinutes != 10 {
		t.Fatalf("событие takeover_reminder: %+v", evs)
	}
	if len(rig.enq.pickups) != 1 || rig.enq.pickups[0].InboundMsgID != 41 {
		t.Fatalf("подхват не взведён: %+v", rig.enq.pickups)
	}
	if d := rig.enq.delays[queue.TypeTakeoverPickup][0]; d != 10*time.Minute {
		t.Errorf("delay подхвата %v, ожидали 10м (settings)", d)
	}
}

// TestTakeoverReminder_NoopWhenManagerReplied — критерий 5: менеджер ответил
// внутри окна → ни уведомления, ни подхвата.
func TestTakeoverReminder_NoopWhenManagerReplied(t *testing.T) {
	author := models.AuthorManagerPrefix + "2"
	history := append(m13History(41, "жду"), models.Message{
		ID: 50, LeadID: 7, Direction: models.DirectionOutbound,
		Author: &author, Content: "здравствуйте, я менеджер",
	})
	rig := newTakeoverRig(m13Lead(models.DialogModeHuman, nil), history)

	if err := rig.handlers.HandleReminder(context.Background(),
		takeoverTask(t, queue.TypeTakeoverReminder, testPayload())); err != nil {
		t.Fatalf("reminder: %v", err)
	}
	if rig.snd.sentCount() != 0 || len(rig.enq.pickups) != 0 || len(rig.pub.events) != 0 {
		t.Error("менеджер ответил — напоминание обязано погаснуть без следов")
	}
}

// TestTakeoverReminder_NoopWhenBotAgain — режим вернули Эмме до срабатывания
// (или пауза истекла) → no-op.
func TestTakeoverReminder_NoopWhenBotAgain(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	for name, lead := range map[string]*models.Lead{
		"режим bot":     m13Lead(models.DialogModeBot, nil),
		"пауза истекла": m13Lead(models.DialogModeBot, &past),
	} {
		t.Run(name, func(t *testing.T) {
			rig := newTakeoverRig(lead, m13History(41, "вопрос"))
			if err := rig.handlers.HandleReminder(context.Background(),
				takeoverTask(t, queue.TypeTakeoverReminder, testPayload())); err != nil {
				t.Fatalf("reminder: %v", err)
			}
			if rig.snd.sentCount() != 0 || len(rig.enq.pickups) != 0 {
				t.Error("Эмма активна — напоминание не нужно")
			}
		})
	}
}

// TestTakeoverReminder_ErasedLead — лид стёрт (LGPD) → задача гаснет молча.
func TestTakeoverReminder_ErasedLead(t *testing.T) {
	rig := newTakeoverRig(m13Lead(models.DialogModeHuman, nil), nil)
	rig.leads = newFakeLeads() // пусто
	rig.handlers.deps.Leads = rig.leads

	if err := rig.handlers.HandleReminder(context.Background(),
		takeoverTask(t, queue.TypeTakeoverReminder, testPayload())); err != nil {
		t.Fatalf("ожидали nil для стёртого лида: %v", err)
	}
	if rig.snd.sentCount() != 0 {
		t.Error("для стёртого лида уведомлений быть не должно")
	}
}

// TestTakeoverPickup_EmmaTakesOver — критерий 4 (вторая половина): менеджер
// так и не ответил → режим bot, пауза и владелец сняты, process:inbound
// поставлен (Эмма ответит штатным контуром), уведомление + WS dialog_mode
// с reason=takeover_pickup.
func TestTakeoverPickup_EmmaTakesOver(t *testing.T) {
	taken := int64(2)
	lead := m13Lead(models.DialogModeHuman, nil)
	lead.TakenBy = &taken
	rig := newTakeoverRig(lead, m13History(41, "ау"))

	err := rig.handlers.HandlePickup(context.Background(),
		takeoverTask(t, queue.TypeTakeoverPickup, testPayload()))
	if err != nil {
		t.Fatalf("pickup: %v", err)
	}
	fresh, _ := rig.leads.GetByID(context.Background(), 7)
	if fresh.DialogMode != models.DialogModeBot || fresh.BotSilencedUntil != nil || fresh.TakenBy != nil {
		t.Errorf("режим не возвращён Эмме: %+v", fresh)
	}
	if len(rig.inbound.calls) != 1 || rig.inbound.calls[0].LeadID != 7 || rig.inbound.calls[0].MsgID != 100 {
		t.Fatalf("process:inbound не поставлен: %+v", rig.inbound.calls)
	}
	evs := rig.pub.byTypeM13(events.TypeDialogMode)
	if len(evs) != 1 || evs[0].Mode != models.DialogModeBot || evs[0].Reason != events.ReasonTakeoverPickup {
		t.Fatalf("событие dialog_mode подхвата: %+v", evs)
	}
	if len(rig.snd.sent) != 1 || !strings.Contains(rig.snd.sent[0].text, "подхватила лид #7") {
		t.Errorf("уведомление о подхвате: %+v", rig.snd.sent)
	}
}

// TestTakeoverPickup_NoopWhenManagerReplied — критерий 5: менеджер успел
// ответить между напоминанием и подхватом → режим НЕ трогается.
func TestTakeoverPickup_NoopWhenManagerReplied(t *testing.T) {
	author := models.AuthorManagerPrefix + "2"
	history := append(m13History(41, "ау"), models.Message{
		ID: 51, LeadID: 7, Direction: models.DirectionOutbound,
		Author: &author, Content: "отвечаю",
	})
	rig := newTakeoverRig(m13Lead(models.DialogModeHuman, nil), history)

	if err := rig.handlers.HandlePickup(context.Background(),
		takeoverTask(t, queue.TypeTakeoverPickup, testPayload())); err != nil {
		t.Fatalf("pickup: %v", err)
	}
	fresh, _ := rig.leads.GetByID(context.Background(), 7)
	if fresh.DialogMode != models.DialogModeHuman {
		t.Error("менеджер ответил — диалог обязан остаться за ним")
	}
	if len(rig.inbound.calls) != 0 || rig.snd.sentCount() != 0 {
		t.Error("no-op не должен ни ставить задач, ни слать уведомлений")
	}
}

// TestTakeoverPickup_NoopWhenBotAgain — подхват уже случился (или режим
// вернули руками): повторный pickup от старого сообщения — no-op, второй
// enqueue process:inbound не делается (защита от двойного ответа Эммы).
func TestTakeoverPickup_NoopWhenBotAgain(t *testing.T) {
	rig := newTakeoverRig(m13Lead(models.DialogModeBot, nil), m13History(41, "ау"))

	if err := rig.handlers.HandlePickup(context.Background(),
		takeoverTask(t, queue.TypeTakeoverPickup, testPayload())); err != nil {
		t.Fatalf("pickup: %v", err)
	}
	if len(rig.inbound.calls) != 0 || rig.snd.sentCount() != 0 || len(rig.pub.events) != 0 {
		t.Error("Эмма уже активна — повторный подхват обязан быть no-op")
	}
}

// TestTakeoverChain_ReminderThenPickup — цепочка LOGIC-01 целиком, вызовы
// напрямую: напоминание → (менеджер молчит) → подхват.
func TestTakeoverChain_ReminderThenPickup(t *testing.T) {
	rig := newTakeoverRig(m13Lead(models.DialogModeHuman, nil), m13History(41, "здравствуйте?"))
	ctx := context.Background()
	p := testPayload()

	if err := rig.handlers.HandleReminder(ctx,
		takeoverTask(t, queue.TypeTakeoverReminder, p)); err != nil {
		t.Fatalf("reminder: %v", err)
	}
	if len(rig.enq.pickups) != 1 {
		t.Fatal("подхват не взведён напоминанием")
	}
	// Менеджер молчит все pickup_minutes → срабатывает подхват.
	if err := rig.handlers.HandlePickup(ctx,
		takeoverTask(t, queue.TypeTakeoverPickup, rig.enq.pickups[0])); err != nil {
		t.Fatalf("pickup: %v", err)
	}
	fresh, _ := rig.leads.GetByID(ctx, 7)
	if fresh.DialogMode != models.DialogModeBot {
		t.Error("после подхвата диалог обязан быть у Эммы")
	}
	if len(rig.inbound.calls) != 1 {
		t.Error("Эмма обязана ответить клиенту штатным контуром process:inbound")
	}
	if rig.snd.sentCount() != 2 { // напоминание + подхват
		t.Errorf("уведомлений менеджеру %d, ожидали 2", rig.snd.sentCount())
	}
}

// TestTakeover_MalformedPayload — битый payload → SkipRetry, как у всех задач.
func TestTakeover_MalformedPayload(t *testing.T) {
	rig := newTakeoverRig(m13Lead(models.DialogModeHuman, nil), nil)
	for _, typ := range []string{queue.TypeTakeoverReminder, queue.TypeTakeoverPickup} {
		task := asynq.NewTask(typ, []byte("не json"))
		var err error
		if typ == queue.TypeTakeoverReminder {
			err = rig.handlers.HandleReminder(context.Background(), task)
		} else {
			err = rig.handlers.HandlePickup(context.Background(), task)
		}
		if err == nil || !strings.Contains(err.Error(), "не разобран") {
			t.Errorf("%s: ожидали ошибку разбора payload, получили %v", typ, err)
		}
	}
}

// byTypeM13 — события заданного типа (fakePublisher живёт в
// kanban_integration_test.go, метода byType у него нет).
func (f *fakePublisher) byTypeM13(typ string) []events.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []events.Event
	for _, ev := range f.events {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}
