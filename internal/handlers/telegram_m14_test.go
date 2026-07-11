// Контрактные тесты M14 (ingestion): язык определяется ОДИН раз — первым
// текстовым inbound; нетекстовое первое сообщение язык не выставляет;
// второе сообщение на другом языке язык в карточке НЕ меняет.
package handlers

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/interfin/interfin-ai-crm/internal/events"
)

func (e *env) languageEvents() []events.Event {
	var out []events.Event
	for _, ev := range e.pub.events {
		if ev.Type == events.TypeLeadLanguage {
			out = append(out, ev)
		}
	}
	return out
}

// nonTextUpdateJSON — апдейт со стикером: message есть, текста/подписи нет.
func nonTextUpdateJSON(tgUserID int64, msgID int) string {
	return fmt.Sprintf(`{
		"update_id": 20,
		"message": {
			"message_id": %d,
			"from": {"id": %d, "first_name": "Ana"},
			"chat": {"id": %d, "type": "private"},
			"sticker": {"file_id": "abc", "width": 512, "height": 512}
		}
	}`, msgID, tgUserID, tgUserID)
}

// TestM14_FirstText_DetectsLanguage — первое текстовое сообщение на испанском:
// язык записан той же транзакцией (CreateInboundSetLanguage), событие
// lead_language ушло с языком целиком.
func TestM14_FirstText_DetectsLanguage(t *testing.T) {
	e := newEnv(t)
	w := e.post(updateJSON(777, 1, "Hola, quiero la ciudadanía"), testSecret)
	if w.Code != http.StatusOK {
		t.Fatalf("ожидали 200, получили %d", w.Code)
	}

	if len(e.msgs.langSets) != 1 || e.msgs.langSets[0] != "es" {
		t.Fatalf("язык при сохранении: %v, ожидали [es]", e.msgs.langSets)
	}
	lead := e.leads.created[0]
	if lead.Language == nil || *lead.Language != "es" {
		t.Fatalf("язык лида: %v, ожидали es", lead.Language)
	}
	evs := e.languageEvents()
	if len(evs) != 1 || evs[0].LeadID != lead.ID || evs[0].Language != "es" {
		t.Fatalf("событие lead_language: %+v", evs)
	}
}

// TestM14_SecondText_DoesNotRedetect — критерий приёмки 2: второе сообщение
// на другом языке язык НЕ меняет (детекция один раз, дальше только менеджер).
func TestM14_SecondText_DoesNotRedetect(t *testing.T) {
	e := newEnv(t)
	e.post(updateJSON(777, 1, "Hi, I need info"), testSecret)
	e.post(updateJSON(777, 2, "Привет, а можно по-русски?"), testSecret)

	if len(e.msgs.langSets) != 1 || e.msgs.langSets[0] != "en" {
		t.Fatalf("записи языка: %v, ожидали одну [en]", e.msgs.langSets)
	}
	lead := e.leads.created[0]
	if lead.Language == nil || *lead.Language != "en" {
		t.Fatalf("язык лида после второго сообщения: %v, ожидали en", lead.Language)
	}
	if evs := e.languageEvents(); len(evs) != 1 {
		t.Fatalf("событий lead_language %d, ожидали 1", len(evs))
	}
	if len(e.msgs.inbound) != 2 {
		t.Fatalf("оба сообщения обязаны сохраниться: %d", len(e.msgs.inbound))
	}
}

// TestM14_NonTextFirst_NoLanguage — критерий приёмки 3 (первая половина):
// первое сообщение нетекстовое — язык не выставлен; первый ТЕКСТ после него
// детектится штатно.
func TestM14_NonTextFirst_NoLanguage(t *testing.T) {
	e := newEnv(t)
	w := e.post(nonTextUpdateJSON(777, 1), testSecret)
	if w.Code != http.StatusOK {
		t.Fatalf("ожидали 200, получили %d", w.Code)
	}
	if len(e.msgs.langSets) != 0 {
		t.Fatalf("нетекстовое первое сообщение не должно выставлять язык: %v", e.msgs.langSets)
	}
	if e.leads.created[0].Language != nil {
		t.Fatalf("язык лида: %v, ожидали NULL", *e.leads.created[0].Language)
	}
	if evs := e.languageEvents(); len(evs) != 0 {
		t.Fatalf("событий lead_language %d, ожидали 0", len(evs))
	}

	// Первый текст после стикера — детекция срабатывает.
	e.post(updateJSON(777, 2, "Buenas tardes, quiero info"), testSecret)
	if len(e.msgs.langSets) != 1 || e.msgs.langSets[0] != "es" {
		t.Fatalf("после первого текста: %v, ожидали [es]", e.msgs.langSets)
	}
}
