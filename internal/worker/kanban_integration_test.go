// Интеграционные тесты M5 против реального Redis + Asynq (REDIS_TEST_ADDR,
// в CI задан): критерии приёмки эпика живьём, через настоящую очередь.
//
//   - IQ-4: TTL Stage 6 сбрасывается при ручном обновлении менеджером
//     (ETA задачи ttl:expire реально сдвигается вперёд);
//   - IQ-9/AQ²-8: 25 inbound → бот молчит + antispam_alert; followup и
//     escalate проезжают через воркер (delay 0 в тестовом конфиге —
//     «24ч/48ч спустя» без ожидания суток); escalated_at записан;
//   - выход из молчания: переход стадии сбрасывает счётчик, бот отвечает —
//     лид не застревает навсегда;
//   - ttl:expire через воркер: стадия 4 с истёкшим TTL → Stage 8.
package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/kanban"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
)

// fakePublisher — сборщик crm:events (сам Redis pub/sub — контур M9).
type fakePublisher struct {
	mu     sync.Mutex
	events []events.Event
}

func (f *fakePublisher) Publish(_ context.Context, ev events.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return nil
}

func (f *fakePublisher) countByType(typ string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, ev := range f.events {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// kanbanTestRig — машина на реальных TTL/anti-spam менеджерах и фейковой БД.
type kanbanTestRig struct {
	machine  *kanban.Machine
	leads    *fakeLeads
	msgs     *fakeMsgs
	snd      *fakeSender
	pub      *fakePublisher
	ttlMgr   *queue.TTLManager
	spamMgr  *queue.AntiSpamManager
	redisCfg config.RedisConfig
}

// newKanbanRig собирает связку. followup/escalate часы = 0: «спустя 24/48ч»
// исполняется немедленно — длительности проверяет queue/antispam_test по ETA.
func newKanbanRig(t *testing.T, lead *models.Lead, followupH, escalateH int) *kanbanTestRig {
	t.Helper()
	redisCfg := testRedis(t)
	cleanQueues(t, redisCfg.Addr)

	rig := &kanbanTestRig{
		leads:    newFakeLeads(lead),
		msgs:     &fakeMsgs{},
		snd:      &fakeSender{},
		pub:      &fakePublisher{},
		redisCfg: redisCfg,
	}
	rig.ttlMgr = queue.NewTTLManager(redisCfg, rig.leads, 0)
	rig.spamMgr = queue.NewAntiSpamManager(redisCfg)
	t.Cleanup(func() {
		rig.ttlMgr.Close()
		rig.spamMgr.Close()
	})
	cfg := config.KanbanConfig{
		AntiSpamLimit:         25,
		AntiSpamFollowupHours: followupH,
		AntiSpamEscalateHours: escalateH,
		TTLStage4Hours:        0, // ttl:expire созревает сразу (см. followup/escalate)
		TTLStage6Days:         5,
	}
	rig.machine = kanban.NewMachine(rig.leads, rig.msgs, rig.ttlMgr, rig.spamMgr,
		rig.pub, rig.snd, cfg, testLogger())
	return rig
}

// startWorker поднимает воркер с обработчиками M5.
func (rig *kanbanTestRig) startWorker(t *testing.T) {
	t.Helper()
	srv := New(rig.redisCfg,
		newTestProcessor(t, rig.leads, rig.msgs, &fakeAI{reply: "ответ"}, rig.snd),
		newTestSummarizer(rig.leads, rig.msgs, &fakeSummaries{}, &fakeAI{}),
		rig.snd, 0, testLogger(),
		func(c *asynq.Config) { c.DelayedTaskCheckInterval = 100 * time.Millisecond })
	srv.RegisterKanban(NewKanbanHandlers(rig.machine, testLogger()))
	if err := srv.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	t.Cleanup(srv.Shutdown)
}

// TestKanban_IQ4_ManualUpdateResetsStage6TTL — IQ-4 живьём: ручное
// обновление менеджером двигает ETA задачи ttl:expire вперёд.
func TestKanban_IQ4_ManualUpdateResetsStage6TTL(t *testing.T) {
	lead := &models.Lead{ID: 61, TelegramUserID: 610, StageID: 2}
	rig := newKanbanRig(t, lead, 24, 48)
	ctx := context.Background()

	// Менеджер отправил предложение: 2 → 6, взводится TTL 5 дней.
	if _, err := rig.machine.Transition(ctx, 61, 6, kanban.ActorManager, "предложение"); err != nil {
		t.Fatal(err)
	}
	insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: rig.redisCfg.Addr})
	defer insp.Close()
	info1, err := insp.GetTaskInfo("default", queue.TTLDedupKey(61))
	if err != nil {
		t.Fatalf("TTL-задача не взведена при входе в стадию 6: %v", err)
	}
	if d := time.Until(info1.NextProcessAt); d < 5*24*time.Hour-time.Minute || d > 5*24*time.Hour+time.Minute {
		t.Fatalf("TTL стадии 6 = %v, ожидали ~5 дней (§3.4)", d)
	}

	// Ручное обновление менеджером спустя паузу: DeleteTask + новый enqueue
	// (CLAUDE.md §4.7) — ETA обязана уехать вперёд на величину паузы.
	// Пауза заметно больше секунды: NextProcessAt в asynq с секундной
	// гранулярностью, сдвиг ровно в 1с не отличим от отсутствия сдвига.
	time.Sleep(2500 * time.Millisecond)
	if err := rig.machine.ResetTTL(ctx, 61); err != nil {
		t.Fatal(err)
	}
	info2, err := insp.GetTaskInfo("default", queue.TTLDedupKey(61))
	if err != nil {
		t.Fatalf("TTL-задача пропала после reset: %v", err)
	}
	if !info2.NextProcessAt.After(info1.NextProcessAt.Add(time.Second)) {
		t.Errorf("ETA не сдвинулась: было %v, стало %v (IQ-4 нарушен)",
			info1.NextProcessAt, info2.NextProcessAt)
	}
	// last_activity_at обновлён — TTL и поле смотрят в одну точку (§3.4).
	got, _ := rig.leads.GetByID(ctx, 61)
	if time.Since(got.LastActivityAt) > time.Minute {
		t.Error("last_activity_at не обновлён при ручном сбросе TTL")
	}
}

// TestKanban_AntiSpamFullCycle — IQ-9 + AQ²-8 живьём: молчание, alert,
// follow-up и эскалация через реальную очередь, выход через менеджера.
func TestKanban_AntiSpamFullCycle(t *testing.T) {
	lead := &models.Lead{ID: 62, TelegramUserID: 620, StageID: 2, MessageCount: 40, AntiSpamCount: 25}
	rig := newKanbanRig(t, lead, 0, 0) // «24ч/48ч» наступают немедленно
	ctx := context.Background()

	// 25-е inbound: бот замолкает, alert уходит, задачи взводятся.
	silenced, err := rig.machine.OnInbound(ctx, lead)
	if err != nil {
		t.Fatal(err)
	}
	if !silenced {
		t.Fatal("25 inbound: бот обязан молчать (IQ-9)")
	}
	if rig.pub.countByType(events.TypeAntiSpamAlert) != 1 {
		t.Fatal("antispam_alert не опубликован (IQ-9: push менеджеру)")
	}

	// Воркер обрабатывает созревшие followup/escalate.
	rig.startWorker(t)
	waitFor(t, 5*time.Second, func() bool {
		rig.snd.mu.Lock()
		defer rig.snd.mu.Unlock()
		return len(rig.snd.sent) >= 1
	}, "follow-up сообщение лиду (§3.5: 24ч)")
	waitFor(t, 5*time.Second, func() bool {
		got, _ := rig.leads.GetByID(ctx, 62)
		return got.EscalatedAt != nil
	}, "эскалация: leads.escalated_at (AQ²-8: 48ч)")
	waitFor(t, 5*time.Second, func() bool {
		return rig.pub.countByType(events.TypeManagerEscalation) >= 1
	}, "событие manager_escalation")

	rig.snd.mu.Lock()
	followupsSent := len(rig.snd.sent)
	rig.snd.mu.Unlock()
	if followupsSent != 1 {
		t.Errorf("follow-up отправлен %d раз, ожидали ровно 1 (§3.5)", followupsSent)
	}

	// Выход из молчания: менеджер двигает стадию → счётчик сброшен, бот
	// снова отвечает. Лид НЕ застрял навсегда (критерий приёмки M5).
	if _, err := rig.machine.Transition(ctx, 62, 5, kanban.ActorManager, "менеджер взял лида"); err != nil {
		t.Fatal(err)
	}
	got, _ := rig.leads.GetByID(ctx, 62)
	if got.AntiSpamCount != 0 {
		t.Fatalf("anti_spam_count=%d после перехода, ожидали 0 (§3.5)", got.AntiSpamCount)
	}
	got.MessageCount++
	got.AntiSpamCount++ // следующее inbound-сообщение в новой стадии
	silenced, err = rig.machine.OnInbound(ctx, got)
	if err != nil {
		t.Fatal(err)
	}
	if silenced {
		t.Error("после сброса бот обязан снова отвечать")
	}
}

// TestKanban_TTLExpireThroughWorker — §3.4 живьём: истёкший TTL стадии 4
// уводит лида в Stage 8 через реальную очередь.
func TestKanban_TTLExpireThroughWorker(t *testing.T) {
	lead := &models.Lead{ID: 63, TelegramUserID: 630, StageID: 2, MessageCount: 10}
	rig := newKanbanRig(t, lead, 24, 48) // TTLStage4Hours=0 → истекает сразу
	ctx := context.Background()

	// Оплата не прошла: payment-вебхук уводит 2 → 4, TTL взводится.
	if _, err := rig.machine.Transition(ctx, 63, 4, kanban.ActorPayment, "underpaid"); err != nil {
		t.Fatal(err)
	}
	rig.startWorker(t)

	waitFor(t, 5*time.Second, func() bool {
		got, _ := rig.leads.GetByID(ctx, 63)
		return got.StageID == 8
	}, "авто-архив по TTL стадии 4 (§3.4)")

	if rig.pub.countByType(events.TypeStageChange) < 2 { // 2→4 и 4→8
		t.Error("переход по TTL не опубликован в crm:events")
	}
}
