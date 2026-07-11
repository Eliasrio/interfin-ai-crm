// Юнит-тесты state machine на фейках — критерии приёмки M5:
//   - авто-переход 1→2 по message_count (только inbound по построению §3.2);
//   - anti-spam: лимит → бот молчит + antispam_alert (IQ-9), follow-up 24ч,
//     эскалация 48ч (AQ²-8), сброс при переходе;
//   - Manual/Payment переопределяют авто-триггеры (CAS + таблица);
//   - TTL: взвод при входе в 4/6, guard устаревших задач, reset вручную (IQ-4).
//
// Интеграционные проверки против реального Redis — queue/antispam_test.go и
// kanban/integration_test.go.
package kanban

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// --- фейки ---

type fakeLeads struct {
	mu      sync.Mutex
	leads   map[int64]*models.Lead
	failCAS bool // имитация проигранной гонки: CAS всегда промахивается
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

func (f *fakeLeads) GetByTelegramUserID(context.Context, int64) (*models.Lead, error) {
	return nil, repo.ErrNotFound
}

func (f *fakeLeads) List(context.Context, repo.ListLeadsParams) ([]models.Lead, int64, error) {
	panic("state machine не листает лидов (метод M8)")
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
	if v, ok := fields["escalated_at"]; ok {
		ts := v.(time.Time)
		lead.EscalatedAt = &ts
	}
	if v, ok := fields["last_activity_at"]; ok {
		lead.LastActivityAt = v.(time.Time)
	}
	if v, ok := fields["ttl_task_id"]; ok {
		if v == nil {
			lead.TTLTaskID = nil
		} else {
			s := v.(string)
			lead.TTLTaskID = &s
		}
	}
	return nil
}

func (f *fakeLeads) TransitionStage(_ context.Context, id int64, from, to int16) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lead, ok := f.leads[id]
	if !ok || f.failCAS || lead.StageID != from {
		return false, nil
	}
	lead.StageID = to
	lead.AntiSpamCount = 0
	lead.LastActivityAt = time.Now() // точка отсчёта TTL (CLAUDE.md §4.7)
	return true, nil
}

func (f *fakeLeads) get(t *testing.T, id int64) models.Lead {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	lead, ok := f.leads[id]
	if !ok {
		t.Fatalf("лид %d потерян фейком", id)
	}
	return *lead
}

type fakeMsgs struct {
	mu       sync.Mutex
	outbound []models.Message
}

func (f *fakeMsgs) CreateInbound(context.Context, *models.Message) error {
	panic("state machine не создаёт inbound")
}

func (f *fakeMsgs) CreateInboundSetLanguage(context.Context, *models.Message, string) (bool, error) {
	panic("state machine не создаёт inbound (детекция языка — контур ingestion M14)")
}

func (f *fakeMsgs) CreateOutbound(_ context.Context, m *models.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m.Direction = models.DirectionOutbound
	f.outbound = append(f.outbound, *m)
	return nil
}

func (f *fakeMsgs) ListByLead(context.Context, int64, int) ([]models.Message, error) {
	return nil, nil
}

func (f *fakeMsgs) ListByLeadBefore(context.Context, int64, int64, int) ([]models.Message, error) {
	return nil, nil
}

func (f *fakeMsgs) HasManagerOutboundAfter(context.Context, int64, int64) (bool, error) {
	return false, nil // контур takeover (M13) в тестах state machine не участвует
}

type ttlCall struct {
	leadID int64
	delay  time.Duration
}

type fakeTTL struct {
	mu        sync.Mutex
	scheduled []ttlCall
	cancelled []int64
	err       error
}

func (f *fakeTTL) Schedule(_ context.Context, leadID int64, delay time.Duration) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scheduled = append(f.scheduled, ttlCall{leadID, delay})
	return nil
}

func (f *fakeTTL) Cancel(_ context.Context, leadID int64) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, leadID)
	return nil
}

type antiSpamCall struct {
	leadID     int64
	stageID    int16
	followupIn time.Duration
	escalateIn time.Duration
}

type fakeAntiSpam struct {
	mu        sync.Mutex
	scheduled []antiSpamCall
	cancelled []int64
	fresh     bool // что вернуть из Schedule
	err       error
}

func (f *fakeAntiSpam) Schedule(_ context.Context, leadID int64, stageID int16, followupIn, escalateIn time.Duration) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scheduled = append(f.scheduled, antiSpamCall{leadID, stageID, followupIn, escalateIn})
	return f.fresh, nil
}

func (f *fakeAntiSpam) Cancel(_ context.Context, leadID int64) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, leadID)
	return nil
}

type fakePub struct {
	mu     sync.Mutex
	events []events.Event
}

func (f *fakePub) Publish(_ context.Context, ev events.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return nil
}

func (f *fakePub) byType(typ string) []events.Event {
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

type sentMsg struct {
	chatID int64
	text   string
}

type fakeSender struct {
	mu   sync.Mutex
	sent []sentMsg
	err  error
}

func (f *fakeSender) Send(chatID int64, text string) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMsg{chatID, text})
	return nil
}

// --- сборка ---

func testCfg() config.KanbanConfig {
	return config.KanbanConfig{
		AntiSpamLimit:         25,
		AntiSpamFollowupHours: 24,
		AntiSpamEscalateHours: 48,
		TTLStage4Hours:        48,
		TTLStage6Days:         5,
	}
}

type fixture struct {
	machine  *Machine
	leads    *fakeLeads
	msgs     *fakeMsgs
	ttl      *fakeTTL
	antiSpam *fakeAntiSpam
	pub      *fakePub
	sender   *fakeSender
}

func newFixture(leads ...*models.Lead) *fixture {
	f := &fixture{
		leads:    newFakeLeads(leads...),
		msgs:     &fakeMsgs{},
		ttl:      &fakeTTL{},
		antiSpam: &fakeAntiSpam{fresh: true},
		pub:      &fakePub{},
		sender:   &fakeSender{},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.machine = NewMachine(f.leads, f.msgs, f.ttl, f.antiSpam, f.pub, f.sender, testCfg(), log)
	return f
}

// --- авто-переход по счётчику (§3.2) ---

func TestOnInbound_AutoAdvance1to2(t *testing.T) {
	lead := &models.Lead{ID: 1, TelegramUserID: 100, StageID: StageGrey, MessageCount: 6, AntiSpamCount: 6}
	fx := newFixture(lead)

	silenced, err := fx.machine.OnInbound(context.Background(), lead)
	if err != nil {
		t.Fatal(err)
	}
	if silenced {
		t.Error("лид замолчан, хотя лимит anti-spam не достигнут")
	}
	if lead.StageID != StageLive {
		t.Errorf("stage = %d, ожидали %d (авто 1→2 на 6-м inbound)", lead.StageID, StageLive)
	}
	if got := fx.leads.get(t, 1); got.StageID != StageLive || got.AntiSpamCount != 0 {
		t.Errorf("в БД stage=%d anti_spam=%d, ожидали %d/0 (сброс §3.5)",
			got.StageID, got.AntiSpamCount, StageLive)
	}
	evs := fx.pub.byType(events.TypeStageChange)
	if len(evs) != 1 {
		t.Fatalf("stage_change событий: %d, ожидали 1 (PUBLISH crm:events)", len(evs))
	}
	if evs[0].StageID != StageLive || evs[0].OldStageID == nil || *evs[0].OldStageID != StageGrey ||
		evs[0].Actor != string(ActorSystem) || evs[0].LeadID != 1 {
		t.Errorf("payload события неверный: %+v", evs[0])
	}
	if len(fx.antiSpam.cancelled) != 1 {
		t.Errorf("anti-spam задачи не сняты при переходе (§3.5): cancel вызван %d раз", len(fx.antiSpam.cancelled))
	}
}

func TestOnInbound_NoAdvanceBelowThreshold(t *testing.T) {
	lead := &models.Lead{ID: 1, StageID: StageGrey, MessageCount: 5}
	fx := newFixture(lead)

	if _, err := fx.machine.OnInbound(context.Background(), lead); err != nil {
		t.Fatal(err)
	}
	if lead.StageID != StageGrey {
		t.Errorf("stage = %d, ожидали %d: 5 inbound ещё «серый» (§3.1)", lead.StageID, StageGrey)
	}
	if len(fx.pub.events) != 0 {
		t.Errorf("событий быть не должно, есть %d", len(fx.pub.events))
	}
}

// --- TTL (§3.4) ---

func TestTransition_EnterTTLStagesSchedulesTTL(t *testing.T) {
	cases := []struct {
		stage int16
		want  time.Duration
	}{
		{StageUnpaid, 48 * time.Hour},
		{StageProposal, 5 * 24 * time.Hour},
	}
	for _, c := range cases {
		lead := &models.Lead{ID: 1, StageID: StageLive}
		fx := newFixture(lead)
		if _, err := fx.machine.Transition(context.Background(), 1, c.stage, ActorManager, "test"); err != nil {
			t.Fatal(err)
		}
		if len(fx.ttl.scheduled) != 1 || fx.ttl.scheduled[0].delay != c.want {
			t.Errorf("stage %d: ttl scheduled=%v, ожидали одну задачу с delay %v",
				c.stage, fx.ttl.scheduled, c.want)
		}
	}
}

func TestTransition_LeaveTTLStageCancelsTTL(t *testing.T) {
	lead := &models.Lead{ID: 1, StageID: StageProposal}
	fx := newFixture(lead)
	if _, err := fx.machine.Transition(context.Background(), 1, StageSold, ActorManager, "продано"); err != nil {
		t.Fatal(err)
	}
	if len(fx.ttl.cancelled) != 1 || len(fx.ttl.scheduled) != 0 {
		t.Errorf("ожидали cancel TTL при уходе из 6 в 7, got scheduled=%v cancelled=%v",
			fx.ttl.scheduled, fx.ttl.cancelled)
	}
}

func TestOnInbound_ActivityResetsTTL(t *testing.T) {
	// §3.4 + CLAUDE.md §4.7: активность лида в TTL-стадии перевзводит TTL
	// (Schedule = DeleteTask + новый enqueue внутри TTLManager).
	lead := &models.Lead{ID: 1, StageID: StageProposal, MessageCount: 30, AntiSpamCount: 3}
	fx := newFixture(lead)
	if _, err := fx.machine.OnInbound(context.Background(), lead); err != nil {
		t.Fatal(err)
	}
	if len(fx.ttl.scheduled) != 1 || fx.ttl.scheduled[0].delay != 5*24*time.Hour {
		t.Errorf("TTL не перевзведён по активности: %v", fx.ttl.scheduled)
	}
}

func TestResetTTL_ManualManagerUpdate(t *testing.T) {
	// IQ-4: ручное обновление менеджером сбрасывает TTL Stage 6.
	lead := &models.Lead{ID: 1, StageID: StageProposal, LastActivityAt: time.Now().Add(-72 * time.Hour)}
	fx := newFixture(lead)
	if err := fx.machine.ResetTTL(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(fx.ttl.scheduled) != 1 || fx.ttl.scheduled[0].delay != 5*24*time.Hour {
		t.Fatalf("TTL не перевзведён: %v", fx.ttl.scheduled)
	}
	if got := fx.leads.get(t, 1); time.Since(got.LastActivityAt) > time.Minute {
		t.Error("last_activity_at не обновлён — TTL считается от него (CLAUDE.md §4.7)")
	}
}

func TestResetTTL_NoTTLStageIsNoop(t *testing.T) {
	lead := &models.Lead{ID: 1, StageID: StageLive}
	fx := newFixture(lead)
	if err := fx.machine.ResetTTL(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(fx.ttl.scheduled)+len(fx.ttl.cancelled) != 0 {
		t.Errorf("стадия 2 без TTL, а менеджер что-то перевзвёл: %v %v",
			fx.ttl.scheduled, fx.ttl.cancelled)
	}
}

func TestHandleTTLExpire_ArchivesFromTTLStages(t *testing.T) {
	for _, stage := range []int16{StageUnpaid, StageProposal} {
		lead := &models.Lead{ID: 1, StageID: stage}
		fx := newFixture(lead)
		if err := fx.machine.HandleTTLExpire(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		if got := fx.leads.get(t, 1); got.StageID != StageFailed {
			t.Errorf("stage %d: после TTL stage=%d, ожидали %d (§3.4)", stage, got.StageID, StageFailed)
		}
		if evs := fx.pub.byType(events.TypeStageChange); len(evs) != 1 || evs[0].Actor != string(ActorTTL) {
			t.Errorf("stage %d: события TTL-перехода нет или актор неверен: %v", stage, evs)
		}
	}
}

func TestHandleTTLExpire_StaleTaskIsNoop(t *testing.T) {
	// Guard: менеджер увёл лида из TTL-стадии, а задача не снялась (гонка).
	lead := &models.Lead{ID: 1, StageID: StageSold}
	fx := newFixture(lead)
	if err := fx.machine.HandleTTLExpire(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if got := fx.leads.get(t, 1); got.StageID != StageSold {
		t.Errorf("устаревший ttl:expire передвинул лида: stage=%d", got.StageID)
	}
	if err := fx.machine.HandleTTLExpire(context.Background(), 404); err != nil {
		t.Errorf("стёртый лид должен быть no-op, получили %v", err)
	}
}

// --- приоритет Manual/Payment над авто (§3.1) ---

func TestPriority_AutoLosesCASRace(t *testing.T) {
	// Авто-триггер посчитан по устаревшему состоянию: CAS промахивается,
	// стадия, выставленная менеджером/оплатой, НЕ перетирается.
	lead := &models.Lead{ID: 1, StageID: StageGrey, MessageCount: 6}
	fx := newFixture(lead)
	fx.leads.failCAS = true // конкурент всегда успевает первым

	silenced, err := fx.machine.OnInbound(context.Background(), lead)
	if err != nil {
		t.Fatalf("проигранная гонка авто-триггера не должна быть ошибкой: %v", err)
	}
	if silenced {
		t.Error("silenced=true без достижения лимита")
	}
	if evs := fx.pub.byType(events.TypeStageChange); len(evs) != 0 {
		t.Errorf("авто-переход опубликовал событие, хотя проиграл CAS: %v", evs)
	}
}

func TestPriority_InvalidAutoTransitionsRejected(t *testing.T) {
	// Авто-акторы жёстко ограничены таблицей — «переопределить» менеджера
	// они не могут даже прямым вызовом Transition.
	lead := &models.Lead{ID: 1, StageID: StageConsultation}
	fx := newFixture(lead)
	if _, err := fx.machine.Transition(context.Background(), 1, StageFailed, ActorSystem, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("system 5→8: ожидали ErrInvalidTransition, получили %v", err)
	}
	if _, err := fx.machine.Transition(context.Background(), 1, StageFailed, ActorTTL, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("ttl 5→8: ожидали ErrInvalidTransition, получили %v", err)
	}
}

func TestPriority_ManagerAndPaymentAllowedWhereAutoIsNot(t *testing.T) {
	lead := &models.Lead{ID: 1, StageID: StageGrey}
	fx := newFixture(lead)
	if _, err := fx.machine.Transition(context.Background(), 1, StageConsultation, ActorManager, "звонок"); err != nil {
		t.Fatalf("manager 1→5: %v", err)
	}
	if _, err := fx.machine.Transition(context.Background(), 1, StagePaid, ActorPayment, "webhook success"); err != nil {
		t.Fatalf("payment 5→3: %v", err)
	}
	if got := fx.leads.get(t, 1); got.StageID != StagePaid {
		t.Errorf("stage=%d, ожидали %d", got.StageID, StagePaid)
	}
}

func TestTransition_SameStageManagerIsNoop(t *testing.T) {
	// Повторный PATCH в ту же стадию: не ошибка, но и не событие. TTL не
	// трогаем — для явного сброса у M8 есть ResetTTL (IQ-4).
	lead := &models.Lead{ID: 1, StageID: StageProposal}
	fx := newFixture(lead)
	if _, err := fx.machine.Transition(context.Background(), 1, StageProposal, ActorManager, "dup"); err != nil {
		t.Fatal(err)
	}
	if len(fx.pub.events) != 0 || len(fx.ttl.scheduled) != 0 || len(fx.antiSpam.cancelled) != 0 {
		t.Errorf("same-stage manager должен быть чистым no-op: events=%v ttl=%v cancels=%v",
			fx.pub.events, fx.ttl.scheduled, fx.antiSpam.cancelled)
	}
}

func TestTransition_RetryAfterSideEffectFailureIsIdempotent(t *testing.T) {
	// CAS прошёл, снятие anti-spam задач упало → ошибка → ретрай доводит
	// side effects, не требуя повторного CAS (лид уже в целевой стадии).
	taskID := "old-ttl-task"
	lead := &models.Lead{ID: 1, StageID: StageUnpaid, TTLTaskID: &taskID}
	fx := newFixture(lead)
	fx.antiSpam.err = errors.New("redis down")

	if err := fx.machine.HandleTTLExpire(context.Background(), 1); err == nil {
		t.Fatal("ожидали ошибку при недоступном anti-spam менеджере")
	}
	if got := fx.leads.get(t, 1); got.StageID != StageFailed {
		t.Fatalf("CAS должен был пройти до падения side effects, stage=%d", got.StageID)
	}

	fx.antiSpam.err = nil // Redis ожил, Asynq ретраит задачу
	if err := fx.machine.HandleTTLExpire(context.Background(), 1); err != nil {
		t.Fatalf("ретрай после восстановления: %v", err)
	}
	if len(fx.antiSpam.cancelled) != 1 {
		t.Errorf("ретрай не довёл снятие anti-spam задач: %v", fx.antiSpam.cancelled)
	}
	got := fx.leads.get(t, 1)
	if got.TTLTaskID != nil {
		t.Error("ttl_task_id не очищен после TTL-перехода")
	}
	// Сама TTL-задача active в момент выполнения — DeleteTask по ней был бы
	// ошибкой asynq (FailedPrecondition), машина не должна её звать.
	if len(fx.ttl.cancelled)+len(fx.ttl.scheduled) != 0 {
		t.Errorf("TTL-переход трогал очередь TTL: cancelled=%v scheduled=%v",
			fx.ttl.cancelled, fx.ttl.scheduled)
	}
}

// --- anti-spam (§3.5, AQ²-8) ---

func TestAntiSpam_LimitSilencesAndArms(t *testing.T) {
	// IQ-9: 25-е inbound → бот молчит + antispam_alert; взводятся
	// followup (24ч) и escalate (48ч).
	lead := &models.Lead{ID: 1, StageID: StageLive, MessageCount: 40, AntiSpamCount: 25}
	fx := newFixture(lead)

	silenced, err := fx.machine.OnInbound(context.Background(), lead)
	if err != nil {
		t.Fatal(err)
	}
	if !silenced {
		t.Error("25 inbound на стадию: бот обязан замолчать (§3.5)")
	}
	if len(fx.antiSpam.scheduled) != 1 {
		t.Fatalf("anti-spam задачи не взведены: %v", fx.antiSpam.scheduled)
	}
	call := fx.antiSpam.scheduled[0]
	if call.followupIn != 24*time.Hour || call.escalateIn != 48*time.Hour || call.stageID != StageLive {
		t.Errorf("параметры взвода неверны: %+v", call)
	}
	alerts := fx.pub.byType(events.TypeAntiSpamAlert)
	if len(alerts) != 1 || alerts[0].LeadID != 1 || alerts[0].AntiSpamCount != 25 {
		t.Errorf("antispam_alert: %v", alerts)
	}
}

func TestAntiSpam_AboveLimitStaysSilentWithoutRearming(t *testing.T) {
	// 26-е сообщение: молчание продолжается, но задачи НЕ перевзводятся —
	// follow-up ровно один (§3.5), отработавший таймер не перезапускается.
	lead := &models.Lead{ID: 1, StageID: StageLive, MessageCount: 41, AntiSpamCount: 26}
	fx := newFixture(lead)

	silenced, err := fx.machine.OnInbound(context.Background(), lead)
	if err != nil {
		t.Fatal(err)
	}
	if !silenced {
		t.Error("молчание должно держаться, пока счётчик за лимитом")
	}
	if len(fx.antiSpam.scheduled) != 0 || len(fx.pub.events) != 0 {
		t.Errorf("перевзвод/повторный alert запрещены: scheduled=%v events=%v",
			fx.antiSpam.scheduled, fx.pub.events)
	}
}

func TestAntiSpam_DuplicateArmDoesNotDuplicateAlert(t *testing.T) {
	// Ретрай/конкурент на пороге: Schedule сообщает fresh=false → alert
	// не публикуется второй раз.
	lead := &models.Lead{ID: 1, StageID: StageLive, MessageCount: 40, AntiSpamCount: 25}
	fx := newFixture(lead)
	fx.antiSpam.fresh = false

	silenced, err := fx.machine.OnInbound(context.Background(), lead)
	if err != nil || !silenced {
		t.Fatalf("silenced=%v err=%v", silenced, err)
	}
	if len(fx.pub.byType(events.TypeAntiSpamAlert)) != 0 {
		t.Error("повторный alert при fresh=false")
	}
}

func TestAntiSpam_TransitionResetsCounterAndTasks(t *testing.T) {
	// §3.5: переход стадии сбрасывает счётчик и снимает задачи — бот снова
	// отвечает.
	lead := &models.Lead{ID: 1, StageID: StageLive, MessageCount: 40, AntiSpamCount: 27}
	fx := newFixture(lead)

	if _, err := fx.machine.Transition(context.Background(), 1, StageConsultation, ActorManager, "менеджер взял"); err != nil {
		t.Fatal(err)
	}
	got := fx.leads.get(t, 1)
	if got.AntiSpamCount != 0 {
		t.Errorf("anti_spam_count=%d, ожидали 0", got.AntiSpamCount)
	}
	if len(fx.antiSpam.cancelled) != 1 {
		t.Errorf("followup/escalate не сняты: %v", fx.antiSpam.cancelled)
	}
	// Следующее inbound в новой стадии — бот не молчит.
	fresh := fx.leads.get(t, 1)
	fresh.MessageCount++
	fresh.AntiSpamCount++
	silenced, err := fx.machine.OnInbound(context.Background(), &fresh)
	if err != nil || silenced {
		t.Errorf("после сброса бот должен отвечать: silenced=%v err=%v", silenced, err)
	}
}

func TestAntiSpam_FollowupSendsExactlyOneMessage(t *testing.T) {
	lead := &models.Lead{ID: 1, TelegramUserID: 500, StageID: StageLive, AntiSpamCount: 25}
	fx := newFixture(lead)

	p := queue.AntiSpamPayload{LeadID: 1, StageID: StageLive}
	if err := fx.machine.HandleAntiSpamFollowup(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(fx.sender.sent) != 1 || fx.sender.sent[0].chatID != 500 {
		t.Fatalf("follow-up не отправлен лиду: %v", fx.sender.sent)
	}
	fx.msgs.mu.Lock()
	saved := len(fx.msgs.outbound)
	fx.msgs.mu.Unlock()
	if saved != 1 {
		t.Errorf("follow-up не сохранён в messages: %d", saved)
	}
}

func TestAntiSpam_StaleTasksAreNoop(t *testing.T) {
	// Стадия сменилась (счётчик сброшен), а задача успела созреть до Cancel —
	// guard по (stage, count) гасит её без действий.
	lead := &models.Lead{ID: 1, TelegramUserID: 500, StageID: StageConsultation, AntiSpamCount: 0}
	fx := newFixture(lead)

	p := queue.AntiSpamPayload{LeadID: 1, StageID: StageLive} // взводилась в стадии 2
	if err := fx.machine.HandleAntiSpamFollowup(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := fx.machine.HandleAntiSpamEscalate(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(fx.sender.sent) != 0 || len(fx.pub.events) != 0 {
		t.Errorf("устаревшие задачи что-то сделали: sent=%v events=%v", fx.sender.sent, fx.pub.events)
	}
	if got := fx.leads.get(t, 1); got.EscalatedAt != nil {
		t.Error("устаревший escalate записал escalated_at")
	}
}

func TestAntiSpam_EscalateMarksLeadAndNotifiesManager(t *testing.T) {
	// AQ²-8, критерий «лид не застревает навсегда»: 48ч молчания →
	// manager_escalation + escalated_at, даже если менеджер проспал alert.
	lead := &models.Lead{ID: 1, TelegramUserID: 500, StageID: StageLive, AntiSpamCount: 30}
	fx := newFixture(lead)

	p := queue.AntiSpamPayload{LeadID: 1, StageID: StageLive}
	if err := fx.machine.HandleAntiSpamEscalate(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := fx.leads.get(t, 1); got.EscalatedAt == nil {
		t.Error("escalated_at не записан (AQ²-8)")
	}
	evs := fx.pub.byType(events.TypeManagerEscalation)
	if len(evs) != 1 || evs[0].LeadID != 1 {
		t.Errorf("manager_escalation не опубликован: %v", evs)
	}
}

// --- ttl_warning (M9): TTL стадии скоро истечёт → событие на доску ---

func TestHandleTTLWarning_PublishesEvent(t *testing.T) {
	fx := newFixture(&models.Lead{ID: 9, TelegramUserID: 900, StageID: StageUnpaid})
	if err := fx.machine.HandleTTLWarning(context.Background(),
		queue.TTLWarnPayload{LeadID: 9, StageID: StageUnpaid}); err != nil {
		t.Fatalf("HandleTTLWarning: %v", err)
	}
	evs := fx.pub.byType(events.TypeTTLWarning)
	if len(evs) != 1 {
		t.Fatalf("ttl_warning событий: %d, ожидали 1", len(evs))
	}
	if evs[0].LeadID != 9 || evs[0].StageID != StageUnpaid {
		t.Fatalf("payload события: %+v", evs[0])
	}
}

// Guard: стадия сменилась в окно между взводом ttl:warn и срабатыванием —
// предупреждение устарело, события нет, ошибки нет.
func TestHandleTTLWarning_StaleStageSkips(t *testing.T) {
	fx := newFixture(&models.Lead{ID: 9, TelegramUserID: 900, StageID: StageLive})
	if err := fx.machine.HandleTTLWarning(context.Background(),
		queue.TTLWarnPayload{LeadID: 9, StageID: StageUnpaid}); err != nil {
		t.Fatalf("HandleTTLWarning: %v", err)
	}
	if evs := fx.pub.byType(events.TypeTTLWarning); len(evs) != 0 {
		t.Fatalf("устаревшее предупреждение опубликовано: %+v", evs)
	}
}

// Лид стёрт (LGPD) — тихий no-op, как у остальных отложенных задач.
func TestHandleTTLWarning_ErasedLeadSkips(t *testing.T) {
	fx := newFixture()
	if err := fx.machine.HandleTTLWarning(context.Background(),
		queue.TTLWarnPayload{LeadID: 404, StageID: StageUnpaid}); err != nil {
		t.Fatalf("HandleTTLWarning по стёртому лиду: %v", err)
	}
	if evs := fx.pub.byType(events.TypeTTLWarning); len(evs) != 0 {
		t.Fatalf("событие по стёртому лиду: %+v", evs)
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
