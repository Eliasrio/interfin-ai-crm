// Интеграционный тест контура takeover (M13, LOGIC-01) против реального
// Redis + Asynq (REDIS_TEST_ADDR, как остальные интеграционные воркера).
//
// Цепочка «напоминание → подхват» по инструкции task-файла: отложенные
// обработчики вызываются НАПРЯМУЮ (без реального ожидания минут), а вот
// постановка задач и финальный ответ Эммы идут через настоящую очередь:
// inbound при human → takeover:reminder в Redis (ETA по settings) →
// HandleReminder → takeover:pickup в Redis → HandlePickup → режим bot +
// process:inbound в Redis → живой воркер отвечает клиенту.
package worker

import (
	"context"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/settings"
)

func TestTakeoverChain_LiveQueue(t *testing.T) {
	redisCfg := testRedis(t)
	cleanQueues(t, redisCfg.Addr)

	lead := m13Lead(models.DialogModeHuman, nil)
	leads := newFakeLeads(lead)
	msgs := &fakeMsgs{history: m13History(41, "здравствуйте, вы тут?")}
	ai := &fakeAI{reply: "здравствуйте! я снова с вами"}
	snd := &fakeSender{}
	pub := &fakePublisher{}

	client := queue.NewClient(redisCfg)
	defer client.Close()
	insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: redisCfg.Addr})
	defer insp.Close()

	// Интервалы «в секундах» смысла не имеют (настройка в минутах) — минуты
	// проверяем по ETA задач в Redis, обработчики зовём напрямую.
	cfg := fakeSettings{settings.KeyReminderMinutes: 2, settings.KeyPickupMinutes: 3}

	proc := NewProcessor(ProcessorDeps{
		Leads: leads, Msgs: msgs, Budgeter: mustBudgeter(t), AI: ai, Sender: snd,
		Settings: cfg, TakeoverEnq: client, Log: testLogger(),
	})
	handlers := NewTakeoverHandlers(TakeoverDeps{
		Leads: leads, Msgs: msgs, Settings: cfg, Enq: client, InboundEnq: client,
		Sender: snd, ManagerChatID: testManagerChat, Pub: pub, Log: testLogger(),
	})

	ctx := context.Background()

	// 1) Inbound при human: Эмма молчит, takeover:reminder реально в Redis
	// с ETA ≈ +2 мин (reminder_minutes из settings).
	if err := proc.HandleProcessInbound(ctx, inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("inbound: %v", err)
	}
	if ai.callCount() != 0 || snd.sentCount() != 0 {
		t.Fatal("в режиме human Эмма обязана молчать")
	}
	reminder, err := insp.GetTaskInfo("default", queue.TakeoverReminderKey(7, 3, 0))
	if err != nil {
		t.Fatalf("takeover:reminder не в очереди: %v", err)
	}
	if eta := time.Until(reminder.NextProcessAt); eta < 90*time.Second || eta > 3*time.Minute {
		t.Errorf("ETA напоминания %v, ожидали ≈2 мин", eta)
	}

	// Дубль апдейта: повторный inbound НЕ плодит второе напоминание (§4.5).
	if err := proc.HandleProcessInbound(ctx, inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("дубль inbound: %v", err)
	}

	// 2) Напоминание сработало (вызов напрямую): уведомление менеджеру +
	// takeover:pickup в Redis с ETA ≈ +3 мин.
	if err := handlers.HandleReminder(ctx,
		asynq.NewTask(queue.TypeTakeoverReminder, reminder.Payload)); err != nil {
		t.Fatalf("reminder: %v", err)
	}
	if snd.sentCount() != 1 || snd.sent[0].chatID != testManagerChat {
		t.Fatalf("уведомление менеджеру: %+v", snd.sent)
	}
	pickup, err := insp.GetTaskInfo("default", queue.TakeoverPickupKey(7, 3))
	if err != nil {
		t.Fatalf("takeover:pickup не в очереди: %v", err)
	}
	if eta := time.Until(pickup.NextProcessAt); eta < 2*time.Minute || eta > 4*time.Minute {
		t.Errorf("ETA подхвата %v, ожидали ≈3 мин", eta)
	}

	// 3) Подхват (вызов напрямую): режим bot, process:inbound в Redis;
	// живой воркер отвечает клиенту штатным контуром M3.
	srv := New(redisCfg,
		proc, newTestSummarizer(leads, msgs, &fakeSummaries{}, ai),
		snd, 0, testLogger())
	if err := srv.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer srv.Shutdown()

	if err := handlers.HandlePickup(ctx,
		asynq.NewTask(queue.TypeTakeoverPickup, pickup.Payload)); err != nil {
		t.Fatalf("pickup: %v", err)
	}
	fresh, _ := leads.GetByID(ctx, 7)
	if fresh.DialogMode != models.DialogModeBot || fresh.TakenBy != nil {
		t.Fatalf("после подхвата: %+v", fresh)
	}
	// Ответ клиенту (chatID лида) через настоящую очередь + уведомление
	// менеджеру о подхвате.
	waitFor(t, 5*time.Second, func() bool {
		snd.mu.Lock()
		defer snd.mu.Unlock()
		toLead := 0
		for _, s := range snd.sent {
			if s.chatID == lead.TelegramUserID {
				toLead++
			}
		}
		return toLead == 1
	}, "ответ Эммы клиенту после подхвата")
	if ai.callCount() != 1 {
		t.Errorf("claude вызван %d раз, ожидали 1 (штатный контур после подхвата)", ai.callCount())
	}
	if pub.countByType("dialog_mode") != 1 {
		t.Errorf("событий dialog_mode %d, ожидали 1 (подхват)", pub.countByType("dialog_mode"))
	}
}
