// Contract-тесты EP-05 (контакты + сценарий панели): ранний выход /start
// (welcome без Claude, порядок ПОСЛЕ Kanban.OnInbound), кнопка менеджера и
// маркер {{handoff}} → режим human + WS client_handoff + уведомление +
// takeover:reminder + emma_events, ветвление клавиатуры Sender, секция
// контактов system-блока и кэш 30 с.
package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
	"github.com/interfin/interfin-ai-crm/internal/settings"
)

// --- фейки EP-05 ---

// fakePanel — PanelSettings без БД: значения из карты, иначе дефолт EP-01.
type fakePanel map[string]string

func (f fakePanel) String(_ context.Context, key string) string {
	if v, ok := f[key]; ok {
		return v
	}
	return settings.StringDefaults[key]
}

// fakeContactsProv — ContactsProvider с фиксированным списком.
type fakeContactsProv []models.EmmaContact

func (f fakeContactsProv) Active(context.Context) ([]models.EmmaContact, error) {
	return f, nil
}

// fakeContactsRepo — in-memory repo.EmmaContactsRepo для тестов кэша
// провайдера (процессору нужен только ListActive).
type fakeContactsRepo struct {
	mu              sync.Mutex
	active          []models.EmmaContact
	err             error
	listActiveCalls int
}

func (f *fakeContactsRepo) List(context.Context) ([]models.EmmaContact, error) {
	return f.ListActive(context.Background())
}
func (f *fakeContactsRepo) GetByID(context.Context, int64) (*models.EmmaContact, error) {
	return nil, repo.ErrNotFound
}
func (f *fakeContactsRepo) Create(context.Context, *models.EmmaContact) error { return nil }
func (f *fakeContactsRepo) Update(context.Context, int64, repo.EmmaContactUpdate) (*models.EmmaContact, error) {
	return nil, repo.ErrNotFound
}
func (f *fakeContactsRepo) Delete(context.Context, int64) error { return nil }
func (f *fakeContactsRepo) ListActive(context.Context) ([]models.EmmaContact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listActiveCalls++
	if f.err != nil {
		return nil, f.err
	}
	return f.active, nil
}

// --- сборка ---

const (
	ep05ManagerChat = int64(555001)
	ep05Button      = "Связаться с менеджером"
)

// ep05Scenario — вкладка 5 «всё включено»: welcome, кнопка, confirm.
func ep05Scenario() fakePanel {
	return fakePanel{
		settings.KeyWelcomeText:          "Здравствуйте! Я Эмма, чем помочь?",
		settings.KeyManagerButtonEnabled: "true",
		settings.KeyManagerButtonText:    ep05Button,
		settings.KeyHandoffConfirmText:   "Сейчас свяжу вас с менеджером, ожидайте",
	}
}

// ep05Lead — свежий лид на каждый тест (фейки мутируют режим).
func ep05Lead(mode string) *models.Lead {
	return &models.Lead{
		ID: 7, TelegramUserID: 424242, StageID: 2, MessageCount: 4, DialogMode: mode,
	}
}

type ep05Rig struct {
	p      *Processor
	leads  *fakeLeads
	msgs   *fakeMsgs
	ai     *fakeAI
	snd    *fakeSender
	enq    *fakeTakeoverEnq
	pub    *fakePublisher
	events *fakeEvents
	kanban *recordingKanban
}

// newEP05Rig — процессор с полным контуром EP-05: панель, Kanban,
// takeover, publisher, журнал событий, чат менеджеров.
func newEP05Rig(t *testing.T, lead *models.Lead, inboundText string, panel fakePanel, ai *fakeAI) *ep05Rig {
	t.Helper()
	rig := &ep05Rig{
		leads: newFakeLeads(lead),
		msgs: &fakeMsgs{history: []models.Message{{
			ID: 90, LeadID: 7, Direction: models.DirectionInbound,
			Content: inboundText, CreatedAt: time.Now().Add(-time.Minute),
		}}},
		ai:     ai,
		snd:    &fakeSender{},
		enq:    newFakeTakeoverEnq(),
		pub:    &fakePublisher{},
		events: &fakeEvents{},
		kanban: &recordingKanban{},
	}
	rig.p = NewProcessor(ProcessorDeps{
		Leads:         rig.leads,
		Msgs:          rig.msgs,
		Budgeter:      mustBudgeter(t),
		AI:            rig.ai,
		Sender:        rig.snd,
		Kanban:        rig.kanban,
		Pub:           rig.pub,
		Settings:      fakeSettings{settings.KeyReminderMinutes: 10},
		TakeoverEnq:   rig.enq,
		Events:        rig.events,
		Panel:         panel,
		ManagerChatID: ep05ManagerChat,
		PublicURL:     "https://crm.example.test",
		Log:           testLogger(),
	})
	return rig
}

// managerNotices — уведомления, ушедшие в чат менеджеров (Send).
func (r *ep05Rig) managerNotices() []sent {
	r.snd.mu.Lock()
	defer r.snd.mu.Unlock()
	var out []sent
	for _, s := range r.snd.sent {
		if s.chatID == ep05ManagerChat {
			out = append(out, s)
		}
	}
	return out
}

func (r *ep05Rig) dialogModeEvents() []events.Event {
	r.pub.mu.Lock()
	defer r.pub.mu.Unlock()
	var out []events.Event
	for _, ev := range r.pub.events {
		if ev.Type == events.TypeDialogMode {
			out = append(out, ev)
		}
	}
	return out
}

func (r *ep05Rig) leadMode(t *testing.T) string {
	t.Helper()
	lead, err := r.leads.GetByID(context.Background(), 7)
	if err != nil {
		t.Fatalf("lead: %v", err)
	}
	return lead.DialogMode
}

// --- ранний выход /start (критерии приёмки 2–3) ---

// Критерий приёмки: /start при непустом welcome → Send(welcome с
// клавиатурой), Claude НЕ вызван, в messages есть outbound welcome,
// Kanban.OnInbound вызван (порядок раннего выхода).
func TestEP05_StartWelcomeEarlyExit(t *testing.T) {
	panel := ep05Scenario()
	rig := newEP05Rig(t, ep05Lead(models.DialogModeBot), "/start", panel, &fakeAI{reply: "не нужен"})

	if err := rig.p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if rig.kanban.inbounds != 1 {
		t.Errorf("Kanban.OnInbound вызван %d раз, ждали 1 (ранний выход ПОСЛЕ него)", rig.kanban.inbounds)
	}
	if rig.ai.callCount() != 0 {
		t.Errorf("Claude вызван %d раз — welcome обязан отвечать без LLM", rig.ai.callCount())
	}
	welcome := panel[settings.KeyWelcomeText]
	if len(rig.msgs.created) != 1 || rig.msgs.created[0].Content != welcome {
		t.Fatalf("outbound welcome в messages: %+v", rig.msgs.created)
	}
	if rig.msgs.created[0].Author == nil || *rig.msgs.created[0].Author != models.AuthorBot {
		t.Errorf("author welcome: %+v", rig.msgs.created[0].Author)
	}
	// Клавиатура — сразу с приветствия (ТЗ §10 п.2).
	if len(rig.snd.keyboard) != 1 || rig.snd.keyboard[0].text != welcome ||
		rig.snd.buttons[0] != ep05Button {
		t.Errorf("SendWithKeyboard: %+v кнопки %v", rig.snd.keyboard, rig.snd.buttons)
	}
	if rig.snd.sentCount() != 0 || len(rig.snd.removed) != 0 {
		t.Errorf("лишние отправки: sent %+v removed %+v", rig.snd.sent, rig.snd.removed)
	}
	if rig.pub.countByType(events.TypeMessage) != 1 {
		t.Errorf("событие message для welcome не опубликовано")
	}
}

// Критерий приёмки: /start при пустом welcome → штатный ответ Эммы
// (поведение как до эпика: Claude вызван).
func TestEP05_StartEmptyWelcomeFallsThrough(t *testing.T) {
	panel := fakePanel{} // все дефолты: welcome пуст, кнопка выключена
	rig := newEP05Rig(t, ep05Lead(models.DialogModeBot), "/start", panel, &fakeAI{reply: "Здравствуйте!"})

	if err := rig.p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if rig.ai.callCount() != 1 {
		t.Errorf("Claude вызван %d раз, ждали 1 (пустой welcome = штатный контур)", rig.ai.callCount())
	}
	if len(rig.msgs.created) != 1 || rig.msgs.created[0].Content != "Здравствуйте!" {
		t.Errorf("outbound: %+v", rig.msgs.created)
	}
}

// --- handoff по маркеру (критерий приёмки 4) ---

// Критерий приёмки: {{handoff}} в ответе Claude → текст ушёл чистым,
// mode=human, DialogModeEvent(client_handoff) опубликован, takeover:reminder
// взведён, emma_events handoff записан, confirm-текст НЕ отправлен вторым
// сообщением.
func TestEP05_HandoffMarker(t *testing.T) {
	panel := ep05Scenario()
	rig := newEP05Rig(t, ep05Lead(models.DialogModeBot), "позовите живого человека",
		panel, &fakeAI{reply: "Конечно, сейчас позову менеджера. {{handoff}}"})

	if err := rig.p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	// Текст клиенту — один и без маркера (кнопка включена → с клавиатурой).
	if len(rig.snd.keyboard) != 1 || rig.snd.keyboard[0].text != "Конечно, сейчас позову менеджера." {
		t.Errorf("текст клиенту: %+v", rig.snd.keyboard)
	}
	// Confirm-текст НЕ ушёл вторым сообщением: единственный outbound — ответ Эммы.
	if len(rig.msgs.created) != 1 || strings.Contains(rig.msgs.created[0].Content, "{{") {
		t.Errorf("messages: %+v", rig.msgs.created)
	}
	if got := rig.leadMode(t); got != models.DialogModeHuman {
		t.Errorf("dialog_mode = %q, ждали human", got)
	}
	dm := rig.dialogModeEvents()
	if len(dm) != 1 || dm[0].Reason != events.ReasonClientHandoff || dm[0].Mode != models.DialogModeHuman {
		t.Errorf("событие dialog_mode: %+v", dm)
	}
	if rig.enq.reminderCount() != 1 {
		t.Errorf("takeover:reminder взведён %d раз, ждали 1", rig.enq.reminderCount())
	}
	if got := rig.events.byType(models.EmmaEventHandoff); len(got) != 1 ||
		got[0].LeadID == nil || *got[0].LeadID != 7 {
		t.Errorf("emma_events handoff: %+v", got)
	}
	if n := rig.managerNotices(); len(n) != 1 || !strings.Contains(n[0].text, "лид #7") ||
		!strings.Contains(n[0].text, "https://crm.example.test/?lead=7") {
		t.Errorf("уведомление менеджерам (текст + deep-link): %+v", n)
	}
}

// --- handoff по кнопке ---

// Кнопка менеджера: ранний выход без Claude, клиенту — настроенный
// confirm-текст (с клавиатурой), режим human, уведомление, reminder, событие.
func TestEP05_ManagerButtonPress(t *testing.T) {
	panel := ep05Scenario()
	rig := newEP05Rig(t, ep05Lead(models.DialogModeBot), "  "+ep05Button+" ", // TrimSpace
		panel, &fakeAI{reply: "не нужен"})

	if err := rig.p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if rig.ai.callCount() != 0 {
		t.Errorf("Claude вызван %d раз — кнопка обязана работать без LLM", rig.ai.callCount())
	}
	confirm := panel[settings.KeyHandoffConfirmText]
	if len(rig.msgs.created) != 1 || rig.msgs.created[0].Content != confirm {
		t.Fatalf("confirm в messages: %+v", rig.msgs.created)
	}
	if len(rig.snd.keyboard) != 1 || rig.snd.keyboard[0].text != confirm {
		t.Errorf("confirm клиенту: %+v", rig.snd.keyboard)
	}
	if got := rig.leadMode(t); got != models.DialogModeHuman {
		t.Errorf("dialog_mode = %q, ждали human", got)
	}
	if dm := rig.dialogModeEvents(); len(dm) != 1 || dm[0].Reason != events.ReasonClientHandoff {
		t.Errorf("событие dialog_mode: %+v", dm)
	}
	if rig.enq.reminderCount() != 1 {
		t.Errorf("takeover:reminder: %d", rig.enq.reminderCount())
	}
	if got := rig.events.byType(models.EmmaEventHandoff); len(got) != 1 {
		t.Errorf("emma_events handoff: %+v", got)
	}
	if n := rig.managerNotices(); len(n) != 1 {
		t.Errorf("уведомление менеджерам: %+v", n)
	}
	if rig.pub.countByType(events.TypeMessage) != 1 {
		t.Errorf("событие message для confirm не опубликовано")
	}
}

// Критерий приёмки: кнопка при уже-human режиме → ранний выход M13 (шаг 2)
// срабатывает раньше панели — дубля уведомления и confirm нет, взведено
// только штатное напоминание M13.
func TestEP05_ButtonWhenAlreadyHuman(t *testing.T) {
	panel := ep05Scenario()
	rig := newEP05Rig(t, ep05Lead(models.DialogModeHuman), ep05Button,
		panel, &fakeAI{reply: "не нужен"})

	if err := rig.p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if rig.ai.callCount() != 0 || len(rig.msgs.created) != 0 {
		t.Errorf("при human не должно быть ни Claude, ни outbound: %+v", rig.msgs.created)
	}
	if n := rig.managerNotices(); len(n) != 0 {
		t.Errorf("дубль уведомления менеджерам: %+v", n)
	}
	if len(rig.dialogModeEvents()) != 0 {
		t.Errorf("дубль события dialog_mode: %+v", rig.dialogModeEvents())
	}
	if got := rig.events.byType(models.EmmaEventHandoff); len(got) != 0 {
		t.Errorf("дубль emma_events handoff: %+v", got)
	}
	// Штатный контур M13: клиент ждёт менеджера — напоминание взведено.
	if rig.enq.reminderCount() != 1 {
		t.Errorf("напоминание M13: %d, ждали 1", rig.enq.reminderCount())
	}
}

// --- клавиатура (критерий приёмки 8) ---

// Критерий приёмки: кнопку выключили → следующий ответ уходит с
// RemoveKeyboard (ветвление Sender-вызовов); включена → каждый ответ с
// клавиатурой.
func TestEP05_KeyboardBranching(t *testing.T) {
	t.Run("выключена → RemoveKeyboard", func(t *testing.T) {
		panel := fakePanel{settings.KeyManagerButtonEnabled: "false"}
		rig := newEP05Rig(t, ep05Lead(models.DialogModeBot), "какие цены?",
			panel, &fakeAI{reply: "Вот цены."})
		if err := rig.p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
			t.Fatalf("handle: %v", err)
		}
		if len(rig.snd.removed) != 1 || rig.snd.removed[0].text != "Вот цены." {
			t.Errorf("RemoveKeyboard: %+v", rig.snd.removed)
		}
		if rig.snd.sentCount() != 0 || len(rig.snd.keyboard) != 0 {
			t.Errorf("лишние отправки: sent %+v keyboard %+v", rig.snd.sent, rig.snd.keyboard)
		}
	})
	t.Run("включена → каждый ответ с клавиатурой", func(t *testing.T) {
		rig := newEP05Rig(t, ep05Lead(models.DialogModeBot), "какие цены?",
			ep05Scenario(), &fakeAI{reply: "Вот цены."})
		if err := rig.p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
			t.Fatalf("handle: %v", err)
		}
		if len(rig.snd.keyboard) != 1 || rig.snd.buttons[0] != ep05Button {
			t.Errorf("SendWithKeyboard: %+v %v", rig.snd.keyboard, rig.snd.buttons)
		}
	})
	t.Run("без панели → старый Send", func(t *testing.T) {
		leads := newFakeLeads(ep05Lead(models.DialogModeBot))
		msgs := &fakeMsgs{history: []models.Message{
			{LeadID: 7, Direction: models.DirectionInbound, Content: "какие цены?"},
		}}
		snd := &fakeSender{}
		p := newTestProcessor(t, leads, msgs, &fakeAI{reply: "Вот цены."}, snd)
		if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
			t.Fatalf("handle: %v", err)
		}
		if snd.sentCount() != 1 || len(snd.keyboard)+len(snd.removed) != 0 {
			t.Errorf("Panel nil обязан слать старым Send: %+v %+v %+v",
				snd.sent, snd.keyboard, snd.removed)
		}
	})
}

// --- секция контактов (критерии приёмки 6–7) ---

func ep05Contact(id int64, name string) models.EmmaContact {
	comment := "давай, когда клиент готов к консультации"
	return models.EmmaContact{
		ID: id, Type: "phone", Name: name, Value: "+7 999 123-45-67",
		Comment: &comment, IsActive: true, SortOrder: int(id),
	}
}

// Критерий приёмки: секция контактов в system-блоке при непустом списке
// (формат ТЗ §3) и отсутствует при пустом; порядок — после стиля/языка,
// перед файлами; инструкция {{handoff}} присутствует всегда.
func TestEP05_ContactsSectionInSystemBlock(t *testing.T) {
	ai := &fakeAI{reply: "Звоните Анне!"}
	rig := newEP05Rig(t, ep05Lead(models.DialogModeBot), "как связаться?", fakePanel{}, ai)
	rig.p.deps.Contacts = fakeContactsProv{ep05Contact(1, "Менеджер Анна")}
	rig.p.deps.FilesProv = NewSendFilesProvider(newFakeSendFiles(pricePDF), testLogger())

	if err := rig.p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	sys := ai.lastSystem()
	for _, want := range []string{
		"Контакты и ссылки (упоминай ТОЛЬКО из этого списка, к месту):",
		"— Менеджер Анна (телефон +7 999 123-45-67): давай, когда клиент готов к консультации",
		"{{handoff}}",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("в system-блоке нет %q;\nsystem: %s", want, sys)
		}
	}
	// Порядок ТЗ §3: контакты перед файлами.
	if strings.Index(sys, "Контакты и ссылки") > strings.Index(sys, "Тебе доступны файлы") {
		t.Errorf("секция контактов обязана идти ПЕРЕД файлами:\n%s", sys)
	}

	// Пустой список → секции нет, инструкция handoff остаётся.
	ai2 := &fakeAI{reply: "ок"}
	rig2 := newEP05Rig(t, ep05Lead(models.DialogModeBot), "как связаться?", fakePanel{}, ai2)
	rig2.p.deps.Contacts = fakeContactsProv{}
	if err := rig2.p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if strings.Contains(ai2.lastSystem(), "Контакты и ссылки") {
		t.Errorf("пустой список дал секцию:\n%s", ai2.lastSystem())
	}
	if !strings.Contains(ai2.lastSystem(), "{{handoff}}") {
		t.Errorf("инструкция handoff пропала при пустых контактах")
	}
}

// Критерий приёмки: PATCH is_active=false убирает контакт из секции ≤30 с
// (кэш провайдера); ошибка БД деградирует до предыдущего списка.
func TestEP05_ContactsProviderCacheTTL(t *testing.T) {
	repoFake := &fakeContactsRepo{active: []models.EmmaContact{
		ep05Contact(1, "Менеджер Анна"), ep05Contact(2, "Офис Búzios"),
	}}
	prov := NewContactsProvider(repoFake, testLogger())
	now := time.Unix(1700000000, 0)
	prov.now = func() time.Time { return now }
	ctx := context.Background()

	first, err := prov.Active(ctx)
	if err != nil || len(first) != 2 {
		t.Fatalf("active: %v, %v", first, err)
	}

	// Контакт выключили — внутри TTL отдаётся кэш…
	repoFake.mu.Lock()
	repoFake.active = repoFake.active[:1]
	repoFake.mu.Unlock()
	now = now.Add(29 * time.Second)
	cached, _ := prov.Active(ctx)
	if len(cached) != 2 || repoFake.listActiveCalls != 1 {
		t.Fatalf("внутри TTL ждали кэш: контактов %d, чтений %d", len(cached), repoFake.listActiveCalls)
	}

	// …после истечения TTL (≤30 с) — свежий список без выключенного.
	now = now.Add(2 * time.Second)
	fresh, _ := prov.Active(ctx)
	if len(fresh) != 1 || fresh[0].Name != "Менеджер Анна" {
		t.Fatalf("после TTL: %+v", fresh)
	}
	if got := contactsSection(fresh); strings.Contains(got, "Búzios") {
		t.Errorf("выключенный контакт остался в секции:\n%s", got)
	}

	// Ошибка БД → предыдущий удачный список, не ошибка.
	repoFake.mu.Lock()
	repoFake.err = errors.New("db down")
	repoFake.mu.Unlock()
	now = now.Add(time.Minute)
	stale, err := prov.Active(ctx)
	if err != nil || len(stale) != 1 {
		t.Fatalf("деградация на протухший кэш: %v, %v", stale, err)
	}
}

// contactsSection: подписи типов и контакт без комментария.
func TestEP05_ContactsSectionFormat(t *testing.T) {
	if got := contactsSection(nil); got != "" {
		t.Fatalf("пустой список обязан давать пустую секцию: %q", got)
	}
	site := models.EmmaContact{Type: "website", Name: "Сайт сервиса", Value: "svoibrazil.ru", IsActive: true}
	got := contactsSection([]models.EmmaContact{site})
	want := "— Сайт сервиса (сайт svoibrazil.ru)"
	if !strings.Contains(got, want) || strings.Contains(got, want+":") {
		t.Errorf("контакт без комментария: %q, ждали %q без двоеточия", got, want)
	}
}
