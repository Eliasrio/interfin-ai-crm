// Контрактные тесты M12 для воркера: реплики менеджера в контексте Эммы
// и событие message на каждый сохранённый outbound.
//
// Решение M12 §7: prompt.go НЕ меняется — outbound менеджера попадает в
// историю через ListByLead и уходит в messages-блок ролью assistant, как
// ответы Эммы («считает слова менеджера своими» и не противоречит им).
package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/interfin/interfin-ai-crm/internal/claude"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/models"
)

// TestPromptBuild_ManagerOutboundInMessages — критерий приёмки M12:
// «Эмма отвечает с учётом реплики менеджера» — сборка контекста кладёт
// outbound менеджера в messages-блок ассистентом, между репликами лида.
func TestPromptBuild_ManagerOutboundInMessages(t *testing.T) {
	manager := models.AuthorManagerPrefix + "5"
	bot := models.AuthorBot
	history := []models.Message{
		{LeadID: 7, Direction: models.DirectionInbound, Content: "Здравствуйте! Сколько стоит сопровождение родов?"},
		{LeadID: 7, Direction: models.DirectionOutbound, Author: &bot, Content: "Добрый день! Уточню детали у старшего менеджера."},
		{LeadID: 7, Direction: models.DirectionInbound, Content: "Хорошо, жду."},
		{LeadID: 7, Direction: models.DirectionOutbound, Author: &manager, Content: "Здравствуйте, это старший менеджер. Для вашей семьи пакет — 2000 USDT."},
		{LeadID: 7, Direction: models.DirectionInbound, Content: "А что входит в эту сумму?"},
	}

	b := newBudgeter(t, &fakeCounter{forbid: true, t: t})
	_, msgs, _, err := b.Build(context.Background(), systemPrompt, "", history)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	found := false
	for _, m := range msgs {
		if strings.Contains(m.Content, "2000 USDT") {
			if m.Role != claude.RoleAssistant {
				t.Fatalf("реплика менеджера ушла ролью %q, ждали assistant", m.Role)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("outbound менеджера отсутствует в messages-блоке контекста Claude")
	}
	// Диалог остался валидным для Messages API: начинается с user,
	// роли чередуются (склейку одинаковых делает toClaudeMessages).
	if msgs[0].Role != claude.RoleUser {
		t.Fatalf("контекст начинается с %q, ждали user", msgs[0].Role)
	}
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == msgs[i-1].Role {
			t.Fatalf("роли не чередуются на позиции %d: %+v", i, msgs)
		}
	}
}

// --- событие message из воркера (вторая точка публикации M12) ---

type workerPub struct {
	events []events.Event
}

func (f *workerPub) Publish(_ context.Context, ev events.Event) error {
	f.events = append(f.events, ev)
	return nil
}

// TestHandle_PublishesMessageEvent — ответ Эммы сохраняется с author=bot
// и уходит событием message (StageID = текущая стадия, stage не трогается).
func TestHandle_PublishesMessageEvent(t *testing.T) {
	leads := newFakeLeads(&models.Lead{ID: 7, TelegramUserID: 424242, StageID: 2})
	msgs := &fakeMsgs{history: []models.Message{
		{LeadID: 7, Direction: models.DirectionInbound, Content: "Здравствуйте!"},
	}}
	ai := &fakeAI{reply: "Добрый день! Я Эмма."}
	pub := &workerPub{}

	p := NewProcessor(ProcessorDeps{
		Leads:    leads,
		Msgs:     msgs,
		Budgeter: mustBudgeter(t),
		AI:       ai,
		Sender:   &fakeSender{},
		Pub:      pub,
		Log:      testLogger(),
	})
	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(msgs.created) != 1 || msgs.created[0].Author == nil ||
		*msgs.created[0].Author != models.AuthorBot {
		t.Fatalf("outbound без author=bot: %+v", msgs.created)
	}
	if len(pub.events) != 1 {
		t.Fatalf("событий %d, ждали 1", len(pub.events))
	}
	ev := pub.events[0]
	if ev.Type != events.TypeMessage || ev.LeadID != 7 || ev.StageID != 2 ||
		ev.Direction != models.DirectionOutbound || ev.Author != models.AuthorBot ||
		ev.Content != ai.reply {
		t.Fatalf("событие message: %+v", ev)
	}
}

// TestHandle_ResendDoesNotRepublish — ретрай упавшего Send (последний в
// истории — outbound) переотправляет сохранённое и НЕ публикует событие
// второй раз: оно ушло при save.
func TestHandle_ResendDoesNotRepublish(t *testing.T) {
	bot := models.AuthorBot
	leads := newFakeLeads(&models.Lead{ID: 7, TelegramUserID: 424242, StageID: 2})
	msgs := &fakeMsgs{history: []models.Message{
		{LeadID: 7, Direction: models.DirectionInbound, Content: "Здравствуйте!"},
		{LeadID: 7, Direction: models.DirectionOutbound, Author: &bot, Content: "Добрый день!"},
	}}
	snd := &fakeSender{}
	pub := &workerPub{}

	p := NewProcessor(ProcessorDeps{
		Leads:    leads,
		Msgs:     msgs,
		Budgeter: mustBudgeter(t),
		AI:       &fakeAI{reply: "не должен вызываться"},
		Sender:   snd,
		Pub:      pub,
		Log:      testLogger(),
	})
	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(snd.sent) != 1 {
		t.Fatalf("переотправка: %+v", snd.sent)
	}
	if len(pub.events) != 0 {
		t.Fatalf("переотправка опубликовала событие повторно: %+v", pub.events)
	}
}
