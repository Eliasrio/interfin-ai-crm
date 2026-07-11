// Контрактные тесты M14 (processor, фейковый Claude): Эмма получает
// языковую инструкцию по языку лида (NULL → ru), детерминированная
// подсказка «понимаю только текст» локализована.
package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/lang"
	"github.com/interfin/interfin-ai-crm/internal/models"
)

// m14Lead — лид с языком (nil = ещё не определён).
func m14Lead(language *string) *models.Lead {
	return &models.Lead{
		ID: 7, TelegramUserID: 424242, StageID: 2, MessageCount: 3,
		DialogMode: models.DialogModeBot, Language: language,
	}
}

func strPtr(s string) *string { return &s }

// TestM14_SystemPromptLanguage — критерий приёмки 1: языковая инструкция
// в system-блоке следует за языком карточки; NULL → русский fallback.
func TestM14_SystemPromptLanguage(t *testing.T) {
	cases := []struct {
		name     string
		language *string
		text     string
		wantWord string
	}{
		{"испанский", strPtr(lang.ES), "Hola, quiero la ciudadanía", "испанском"},
		{"английский", strPtr(lang.EN), "Hi, I need info", "английском"},
		{"русский", strPtr(lang.RU), "Привет", "русском"},
		{"NULL → русский", nil, "Привет", "русском"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			leads := newFakeLeads(m14Lead(c.language))
			msgs := &fakeMsgs{history: []models.Message{{
				ID: 41, LeadID: 7, Direction: models.DirectionInbound,
				Content: c.text, CreatedAt: time.Now().Add(-time.Minute),
			}}}
			ai := &fakeAI{reply: "ответ Эммы"}
			snd := &fakeSender{}

			err := newTestProcessor(t, leads, msgs, ai, snd).
				HandleProcessInbound(context.Background(), inboundTask(t, 7, 100))
			if err != nil {
				t.Fatalf("handle: %v", err)
			}
			system := ai.lastSystem()
			if !strings.Contains(system, "Отвечай на "+c.wantWord) {
				t.Errorf("system-блок без языковой инструкции %q:\n%s", c.wantWord, system)
			}
			if strings.Contains(system, "Отвечай на русском, тепло") {
				t.Error("старое правило «Отвечай на русском» обязано уйти из systemPrompt")
			}
			if snd.sentCount() != 1 {
				t.Errorf("ответ не отправлен: %d", snd.sentCount())
			}
		})
	}
}

// TestM14_NonTextReplyLocalized — критерий приёмки 3: подсказка «понимаю
// только текст» на языке лида; NULL → русская (первое сообщение — голосовое:
// язык ещё не выставлен ingestion'ом).
func TestM14_NonTextReplyLocalized(t *testing.T) {
	cases := []struct {
		name     string
		language *string
		want     string
	}{
		{"первое голосовое, язык не определён", nil, nonTextReplies[lang.RU]},
		{"стикер от испаноязычного", strPtr(lang.ES), nonTextReplies[lang.ES]},
		{"стикер от англоязычного", strPtr(lang.EN), nonTextReplies[lang.EN]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			leads := newFakeLeads(m14Lead(c.language))
			msgs := &fakeMsgs{history: []models.Message{{
				ID: 42, LeadID: 7, Direction: models.DirectionInbound,
				Content: "", CreatedAt: time.Now(),
			}}}
			ai := &fakeAI{reply: "не должно понадобиться"}
			snd := &fakeSender{}

			err := newTestProcessor(t, leads, msgs, ai, snd).
				HandleProcessInbound(context.Background(), inboundTask(t, 7, 101))
			if err != nil {
				t.Fatalf("handle: %v", err)
			}
			if ai.callCount() != 0 {
				t.Errorf("claude вызван %d раз, ожидали 0", ai.callCount())
			}
			if len(snd.sent) != 1 || snd.sent[0].text != c.want {
				t.Errorf("подсказка: %+v, ожидали %q", snd.sent, c.want)
			}
			if len(msgs.created) != 1 || msgs.created[0].Content != c.want {
				t.Errorf("сохранённый outbound: %+v", msgs.created)
			}
		})
	}
}

// TestM14_NonTextRetryResendsSameLocalized — ретрай после упавшего Send
// переотправляет СОХРАНЁННУЮ локализованную подсказку (ветка переотправки,
// Claude не вызывается).
func TestM14_NonTextRetryResendsSameLocalized(t *testing.T) {
	leads := newFakeLeads(m14Lead(strPtr(lang.ES)))
	msgs := &fakeMsgs{history: []models.Message{{
		ID: 43, LeadID: 7, Direction: models.DirectionInbound,
		Content: "", CreatedAt: time.Now(),
	}}}
	snd := &fakeSender{sendErr: errors.New("telegram: 502")}
	ai := &fakeAI{}
	p := newTestProcessor(t, leads, msgs, ai, snd)
	task := inboundTask(t, 7, 102)

	// Попытка 1: подсказка сохранена, Send упал.
	if err := p.HandleProcessInbound(context.Background(), task); err == nil {
		t.Fatal("первая попытка обязана вернуть ошибку Send")
	}
	// Попытка 2 (ретрай): переотправка сохранённого испанского текста.
	if err := p.HandleProcessInbound(context.Background(), task); err != nil {
		t.Fatalf("ретрай: %v", err)
	}
	if ai.callCount() != 0 {
		t.Errorf("claude вызван %d раз, ожидали 0", ai.callCount())
	}
	if len(msgs.created) != 1 || msgs.created[0].Content != nonTextReplies[lang.ES] {
		t.Errorf("outbound-строк %d (%+v), ожидали одну испанскую подсказку",
			len(msgs.created), msgs.created)
	}
	if snd.sentCount() != 1 || snd.sent[0].text != nonTextReplies[lang.ES] {
		t.Errorf("переотправка: %+v", snd.sent)
	}
}
