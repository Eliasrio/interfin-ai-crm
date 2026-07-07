// Контрактные тесты чата менеджера и счёта из карточки (M12) — на общем
// риге api_contract_test.go (rate limit → JWT → роли, фейки in-memory).
// Ключевой инвариант: отказ Telegram → 502 ERR_TELEGRAM_SEND и в messages
// НИЧЕГО не пишется — в истории нет недоставленных реплик.
package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/payment"
)

// chatSender — фейковый TelegramSender: пишет доставки, умеет отказывать.
type chatSender struct {
	sent []struct {
		ChatID int64
		Text   string
	}
	err error
}

func (s *chatSender) Send(chatID int64, text string) error {
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, struct {
		ChatID int64
		Text   string
	}{chatID, text})
	return nil
}

// chatInvoices — фейковый InvoiceCreator (контур M6 в тестах не трогаем).
type chatInvoices struct {
	params  []payment.CreateInvoiceParams
	leadIDs []int64
	err     error
}

func (f *chatInvoices) CreateInvoice(_ context.Context, leadID int64, p payment.CreateInvoiceParams) (*payment.Invoice, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.params = append(f.params, p)
	f.leadIDs = append(f.leadIDs, leadID)
	return &payment.Invoice{
		InvoiceID:     555,
		Status:        "active",
		Asset:         p.Asset,
		Amount:        p.Amount,
		Payload:       p.Payload,
		BotInvoiceURL: "https://t.me/CryptoTestnetBot?start=IVoJp555",
	}, nil
}

func seedChatHistory(rig *apiRig, leadID int64, n int) {
	bot := models.AuthorBot
	for i := 1; i <= n; i++ {
		dir, author := models.DirectionInbound, (*string)(nil)
		if i%2 == 0 {
			dir, author = models.DirectionOutbound, &bot
		}
		rig.store.msgSeq++
		rig.store.msgs[leadID] = append(rig.store.msgs[leadID], models.Message{
			ID: rig.store.msgSeq, LeadID: leadID, Direction: dir, Author: author,
			Content: "сообщение", Tokens: nil,
		})
	}
}

// --- GET /api/leads/:id/messages ---

func TestChatListMessages(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	seedChatHistory(rig, 1, 7)
	token := rig.token(t, auth.RoleManager)

	// Без параметров — последние (все 7), от старых к новым.
	m := wantStatus(t, rig.do(t, http.MethodGet, "/api/leads/1/messages", token, nil), http.StatusOK, "")
	msgs := m["messages"].([]any)
	if len(msgs) != 7 {
		t.Fatalf("messages = %d, ждали 7", len(msgs))
	}
	first := msgs[0].(map[string]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if first["id"].(float64) != 1 || last["id"].(float64) != 7 {
		t.Fatalf("порядок не «старые → новые»: %v .. %v", first["id"], last["id"])
	}
	if _, ok := first["author"]; !ok {
		t.Fatal("в DTO сообщения нет поля author (M12)")
	}

	// limit — последние 3 (id 5..7).
	m = wantStatus(t, rig.do(t, http.MethodGet, "/api/leads/1/messages?limit=3", token, nil), http.StatusOK, "")
	msgs = m["messages"].([]any)
	if len(msgs) != 3 || msgs[0].(map[string]any)["id"].(float64) != 5 {
		t.Fatalf("limit=3: ждали id 5..7, получили %v", msgs)
	}

	// before_id листает назад: страница до id=5 — это 2..4 при limit=3.
	m = wantStatus(t, rig.do(t, http.MethodGet, "/api/leads/1/messages?limit=3&before_id=5", token, nil),
		http.StatusOK, "")
	msgs = m["messages"].([]any)
	if len(msgs) != 3 ||
		msgs[0].(map[string]any)["id"].(float64) != 2 ||
		msgs[2].(map[string]any)["id"].(float64) != 4 {
		t.Fatalf("before_id=5&limit=3: ждали id 2..4, получили %v", msgs)
	}

	// Валидация — 400.
	for _, q := range []string{"?limit=0", "?limit=201", "?limit=abc", "?before_id=0", "?before_id=x"} {
		wantStatus(t, rig.do(t, http.MethodGet, "/api/leads/1/messages"+q, token, nil),
			http.StatusBadRequest, "ERR_VALIDATION")
	}

	// Нет лида — 404; стёртый — тоже 404 (как M8).
	wantStatus(t, rig.do(t, http.MethodGet, "/api/leads/999/messages", token, nil),
		http.StatusNotFound, "ERR_NOT_FOUND")
	wantStatus(t, rig.do(t, http.MethodDelete, "/api/lgpd/leads/1/erase", token, nil),
		http.StatusOK, "")
	wantStatus(t, rig.do(t, http.MethodGet, "/api/leads/1/messages", token, nil),
		http.StatusNotFound, "ERR_NOT_FOUND")
}

// --- POST /api/leads/:id/messages ---

func TestChatPostMessage(t *testing.T) {
	lead := testLead(1, 2)
	rig := newAPIRig(t, lead)
	token := rig.token(t, auth.RoleManager) // sub="1" (rig.token)

	m := wantStatus(t, rig.do(t, http.MethodPost, "/api/leads/1/messages", token,
		gin.H{"text": "  Добрый день! Подхватываю диалог.  "}), http.StatusOK, "")

	// Доставлено в Telegram по chat id лида, текст без краевых пробелов.
	if len(rig.sender.sent) != 1 || rig.sender.sent[0].ChatID != lead.TelegramUserID ||
		rig.sender.sent[0].Text != "Добрый день! Подхватываю диалог." {
		t.Fatalf("telegram send: %+v", rig.sender.sent)
	}

	// Строка в messages: outbound, author=manager:<sub>, счётчики не тронуты.
	stored := rig.store.msgs[1]
	if len(stored) != 1 {
		t.Fatalf("в messages %d строк, ждали 1", len(stored))
	}
	if stored[0].Direction != models.DirectionOutbound ||
		stored[0].Author == nil || *stored[0].Author != "manager:1" {
		t.Fatalf("outbound сохранён неверно: %+v", stored[0])
	}
	if rig.store.leads[1].MessageCount != testLead(1, 2).MessageCount {
		t.Fatal("message_count изменился: считаются только inbound (CLAUDE.md §4.3)")
	}

	// Ответ — DTO сообщения; WS-событие message опубликовано.
	dto := m["message"].(map[string]any)
	if dto["direction"] != "outbound" || dto["author"] != "manager:1" {
		t.Fatalf("DTO ответа: %v", dto)
	}
	if len(rig.pub.events) != 1 {
		t.Fatalf("событий %d, ждали 1", len(rig.pub.events))
	}
	ev := rig.pub.events[0]
	if ev.Type != events.TypeMessage || ev.LeadID != 1 || ev.Author != "manager:1" ||
		ev.Direction != models.DirectionOutbound || ev.StageID != lead.StageID {
		t.Fatalf("событие message: %+v", ev)
	}
}

func TestChatPostMessageTelegramFail(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	rig.sender.err = errors.New("telegram: send: forbidden: bot was blocked by the user")
	token := rig.token(t, auth.RoleManager)

	wantStatus(t, rig.do(t, http.MethodPost, "/api/leads/1/messages", token,
		gin.H{"text": "привет"}), http.StatusBadGateway, "ERR_TELEGRAM_SEND")

	// Ключевой инвариант M12: в БД ничего, событий нет.
	if len(rig.store.msgs[1]) != 0 {
		t.Fatalf("недоставленная реплика попала в messages: %+v", rig.store.msgs[1])
	}
	if len(rig.pub.events) != 0 {
		t.Fatalf("событие о недоставленной реплике: %+v", rig.pub.events)
	}
}

func TestChatPostMessageValidation(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	token := rig.token(t, auth.RoleManager)

	for name, body := range map[string]any{
		"без text":       gin.H{},
		"пустой":         gin.H{"text": ""},
		"пробельный":     gin.H{"text": "   \n\t "},
		"длиннее 4096":   gin.H{"text": strings.Repeat("ю", 4097)},
		"text не строка": gin.H{"text": 42},
	} {
		w := rig.do(t, http.MethodPost, "/api/leads/1/messages", token, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: статус %d, ждали 400; тело: %s", name, w.Code, w.Body.String())
		}
	}
	// Ровно 4096 символов (не байт!) — валидно.
	wantStatus(t, rig.do(t, http.MethodPost, "/api/leads/1/messages", token,
		gin.H{"text": strings.Repeat("ю", 4096)}), http.StatusOK, "")

	wantStatus(t, rig.do(t, http.MethodPost, "/api/leads/999/messages", token,
		gin.H{"text": "привет"}), http.StatusNotFound, "ERR_NOT_FOUND")
}

// --- POST /api/leads/:id/invoice ---

func TestChatInvoice(t *testing.T) {
	lead := testLead(1, 5)
	rig := newAPIRig(t, lead)
	token := rig.token(t, auth.RoleManager)

	m := wantStatus(t, rig.do(t, http.MethodPost, "/api/leads/1/invoice", token,
		gin.H{"amount": "2000", "asset": "usdt", "description": "Пакет «Роды под ключ»"}),
		http.StatusOK, "")

	// Ответ менеджеру — {invoice_id, url} (показать в UI).
	if m["invoice_id"].(float64) != 555 || !strings.HasPrefix(m["url"].(string), "https://t.me/") {
		t.Fatalf("ответ инвойса: %v", m)
	}

	// Инвойс создан контуром M6: lead_id, 24 часа, decimal-сумма, аппер-кейс asset.
	if len(rig.invoices.params) != 1 || rig.invoices.leadIDs[0] != 1 {
		t.Fatalf("createInvoice: %+v %v", rig.invoices.params, rig.invoices.leadIDs)
	}
	p := rig.invoices.params[0]
	if p.Asset != "USDT" || !p.Amount.Equal(decimal.NewFromInt(2000)) ||
		p.ExpiresIn != 24*3600 || p.Description != "Пакет «Роды под ключ»" {
		t.Fatalf("параметры инвойса: %+v", p)
	}

	// Клиенту ушло сообщение со ссылкой; строка в messages (author=manager:1);
	// WS-событие message опубликовано.
	if len(rig.sender.sent) != 1 || !strings.Contains(rig.sender.sent[0].Text, m["url"].(string)) ||
		!strings.Contains(rig.sender.sent[0].Text, "2000 USDT") {
		t.Fatalf("сообщение клиенту: %+v", rig.sender.sent)
	}
	stored := rig.store.msgs[1]
	if len(stored) != 1 || stored[0].Author == nil || *stored[0].Author != "manager:1" ||
		!strings.Contains(stored[0].Content, m["url"].(string)) {
		t.Fatalf("outbound со ссылкой: %+v", stored)
	}
	if len(rig.pub.events) != 1 || rig.pub.events[0].Type != events.TypeMessage {
		t.Fatalf("события: %+v", rig.pub.events)
	}
}

func TestChatInvoiceTelegramFailReturnsURL(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 5))
	rig.sender.err = errors.New("telegram: send: forbidden")
	token := rig.token(t, auth.RoleManager)

	// Счёт уже создан → 502, но url в теле: менеджер перешлёт ссылку сам.
	m := wantStatus(t, rig.do(t, http.MethodPost, "/api/leads/1/invoice", token,
		gin.H{"amount": "100", "asset": "USDC"}), http.StatusBadGateway, "ERR_TELEGRAM_SEND")
	if !strings.HasPrefix(m["url"].(string), "https://t.me/") || m["invoice_id"].(float64) != 555 {
		t.Fatalf("в 502 нет живой ссылки: %v", m)
	}
	// В БД сообщения нет, событий нет.
	if len(rig.store.msgs[1]) != 0 || len(rig.pub.events) != 0 {
		t.Fatalf("недоставленное сообщение оставило след: %+v %+v",
			rig.store.msgs[1], rig.pub.events)
	}
}

func TestChatInvoiceGatewayFail(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 5))
	rig.invoices.err = errors.New("payment: createInvoice: crypto pay ошибка 401 UNAUTHORIZED")
	token := rig.token(t, auth.RoleManager)

	wantStatus(t, rig.do(t, http.MethodPost, "/api/leads/1/invoice", token,
		gin.H{"amount": "100", "asset": "USDT"}), http.StatusBadGateway, "ERR_INVOICE_CREATE")
	if len(rig.sender.sent) != 0 || len(rig.store.msgs[1]) != 0 {
		t.Fatal("без инвойса не должно быть ни отправки, ни записи")
	}
}

func TestChatInvoiceValidation(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 5))
	token := rig.token(t, auth.RoleManager)

	for name, body := range map[string]any{
		"amount мусор":         gin.H{"amount": "две тыщи", "asset": "USDT"},
		"amount отрицательный": gin.H{"amount": "-5", "asset": "USDT"},
		"amount ноль":          gin.H{"amount": "0", "asset": "USDT"},
		"amount числом":        gin.H{"amount": 2000, "asset": "USDT"},
		"asset не из списка":   gin.H{"amount": "100", "asset": "TON"},
		"без полей":            gin.H{},
		"description длинный":  gin.H{"amount": "100", "asset": "USDT", "description": strings.Repeat("ю", 1025)},
	} {
		w := rig.do(t, http.MethodPost, "/api/leads/1/invoice", token, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: статус %d, ждали 400; тело: %s", name, w.Code, w.Body.String())
		}
	}

	wantStatus(t, rig.do(t, http.MethodPost, "/api/leads/999/invoice", token,
		gin.H{"amount": "100", "asset": "USDT"}), http.StatusNotFound, "ERR_NOT_FOUND")

	// Инвойс не создавался ни разу — валидация стоит ДО похода в шлюз.
	if len(rig.invoices.params) != 0 {
		t.Fatalf("невалидный запрос дошёл до шлюза: %+v", rig.invoices.params)
	}
}
