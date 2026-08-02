// publiclead_test.go — контракт публичного приёма заявок с сайта:
// валидация, дедуп по телефону, CORS, rate limit, деградации (Telegram/БД).
// Фейки — общие для пакета: fakeLeads/fakeMsgs (telegram_test.go),
// chatSender (chat_test.go), fakePub (payment_test.go).
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/events"
)

const (
	publicTestOrigin  = "https://borninbrazil.baby"
	publicTestChatID  = int64(777)
	publicTestBaseURL = "https://crm.borninbrazil.baby"
)

// publicLimiter — управляемый лимитер: фиксирует ключи, умеет отказывать
// и падать (fail-open ветка).
type publicLimiter struct {
	keys  []string
	allow bool
	err   error
}

func (l *publicLimiter) Allow(_ context.Context, key string) (bool, error) {
	l.keys = append(l.keys, key)
	return l.allow, l.err
}

type publicEnv struct {
	router  *gin.Engine
	leads   *fakeLeads
	msgs    *fakeMsgs
	sender  *chatSender
	pub     *fakePub
	limiter *publicLimiter
}

func newPublicEnv(t *testing.T) *publicEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := &publicEnv{
		leads:   newFakeLeads(),
		msgs:    &fakeMsgs{},
		sender:  &chatSender{},
		pub:     &fakePub{},
		limiter: &publicLimiter{allow: true},
	}
	e.router = gin.New()
	NewPublicLead(PublicLeadDeps{
		Leads:          e.leads,
		Msgs:           e.msgs,
		Sender:         e.sender,
		Pub:            e.pub,
		Limiter:        e.limiter,
		AllowedOrigins: []string{publicTestOrigin, "http://localhost:8791"},
		ManagerChatID:  publicTestChatID,
		PublicURL:      publicTestBaseURL,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Register(e.router)
	return e
}

// leadJSON — заявка формы сайта, как её шлёт index.html.
func leadJSON(overrides map[string]string) string {
	m := map[string]string{
		"name":      "Мария Иванова",
		"phone":     "+55 (21) 99999-9999",
		"messenger": "WhatsApp",
		"due_date":  "2026-11-01",
		"city":      "Рио-де-Жанейро",
		"package":   "Комфорт",
		"message":   "Хотим приехать в октябре",
		"lang":      "ru",
		"source":    "borninbrazil.baby",
	}
	for k, v := range overrides {
		if v == "" {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	raw, _ := json.Marshal(m)
	return string(raw)
}

func (e *publicEnv) post(t *testing.T, origin, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/public/lead", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func TestPublicLead_Success(t *testing.T) {
	e := newPublicEnv(t)
	w := e.post(t, publicTestOrigin, leadJSON(nil))

	if w.Code != http.StatusOK {
		t.Fatalf("статус = %d, тело %s; ожидался 200", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != publicTestOrigin {
		t.Errorf("Access-Control-Allow-Origin = %q, ожидался %q", got, publicTestOrigin)
	}

	// Лид: синтетический ОТРИЦАТЕЛЬНЫЙ telegram_user_id, стадия 1, согласие.
	if len(e.leads.created) != 1 {
		t.Fatalf("создано лидов = %d, ожидался 1", len(e.leads.created))
	}
	lead := e.leads.created[0]
	if lead.TelegramUserID >= 0 {
		t.Errorf("telegram_user_id = %d, ожидался отрицательный (синтетический)", lead.TelegramUserID)
	}
	if lead.StageID != 1 {
		t.Errorf("stage_id = %d, ожидался 1", lead.StageID)
	}
	if lead.Name == nil || *lead.Name != "Мария Иванова" {
		t.Errorf("name = %v", lead.Name)
	}
	if lead.Phone == nil || *lead.Phone != "+55 (21) 99999-9999" {
		t.Errorf("phone = %v", lead.Phone)
	}
	if lead.ConsentGivenAt == nil {
		t.Error("consent_given_at не выставлен — форма и есть согласие на контакт (LGPD)")
	}
	if lead.Language == nil || *lead.Language != "ru" {
		t.Errorf("language = %v, ожидался ru", lead.Language)
	}

	// Inbound-сообщение с текстом заявки.
	if len(e.msgs.inbound) != 1 {
		t.Fatalf("inbound-сообщений = %d, ожидалось 1", len(e.msgs.inbound))
	}
	content := e.msgs.inbound[0].Content
	for _, want := range []string{"Мария Иванова", "+55 (21) 99999-9999", "Комфорт", "Рио-де-Жанейро", "WhatsApp", "2026-11-01", "Хотим приехать"} {
		if !strings.Contains(content, want) {
			t.Errorf("в тексте заявки нет %q:\n%s", want, content)
		}
	}

	// Событие message — карточка всплывает на доске (fetchUnknownLead).
	if len(e.pub.events) != 1 || e.pub.events[0].Type != events.TypeMessage ||
		e.pub.events[0].LeadID != lead.ID {
		t.Errorf("события = %+v, ожидалось одно message по лиду %d", e.pub.events, lead.ID)
	}

	// Уведомление менеджеру: тот чат, полный текст, deep-link на карточку.
	if len(e.sender.sent) != 1 {
		t.Fatalf("отправок менеджеру = %d, ожидалась 1", len(e.sender.sent))
	}
	note := e.sender.sent[0]
	if note.ChatID != publicTestChatID {
		t.Errorf("chat_id уведомления = %d, ожидался %d", note.ChatID, publicTestChatID)
	}
	for _, want := range []string{"Новая заявка", "Мария Иванова", "+55 (21) 99999-9999", "?lead="} {
		if !strings.Contains(note.Text, want) {
			t.Errorf("в уведомлении нет %q:\n%s", want, note.Text)
		}
	}
}

func TestPublicLead_Validation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"без телефона", leadJSON(map[string]string{"phone": ""})},
		{"без имени", leadJSON(map[string]string{"name": ""})},
		{"телефон без цифр", leadJSON(map[string]string{"phone": "позвоните сами"})},
		{"невалидный JSON", "{не json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPublicEnv(t)
			w := e.post(t, publicTestOrigin, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("статус = %d, ожидался 400", w.Code)
			}
			if !strings.Contains(w.Body.String(), codeValidation) {
				t.Errorf("тело без %s: %s", codeValidation, w.Body.String())
			}
			if len(e.leads.created) != 0 || len(e.msgs.inbound) != 0 || len(e.sender.sent) != 0 {
				t.Error("невалидная заявка не должна ничего создавать и слать")
			}
		})
	}
}

// TestPublicLead_DuplicatePhone — повторная заявка с тем же номером (в другом
// форматировании) не плодит карточку: сообщение и уведомление дописываются
// в существующего лида.
func TestPublicLead_DuplicatePhone(t *testing.T) {
	e := newPublicEnv(t)
	if w := e.post(t, publicTestOrigin, leadJSON(nil)); w.Code != http.StatusOK {
		t.Fatalf("первая заявка: статус %d", w.Code)
	}
	w := e.post(t, publicTestOrigin, leadJSON(map[string]string{
		"phone":   "5521999999999", // те же цифры без скобок и плюса
		"package": "Премиум",
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("повторная заявка: статус %d", w.Code)
	}
	if len(e.leads.created) != 1 {
		t.Fatalf("создано лидов = %d, ожидался 1 (дедуп по телефону)", len(e.leads.created))
	}
	if len(e.msgs.inbound) != 2 {
		t.Fatalf("inbound-сообщений = %d, ожидалось 2", len(e.msgs.inbound))
	}
	if e.msgs.inbound[1].LeadID != e.leads.created[0].ID {
		t.Error("второе сообщение ушло не в существующего лида")
	}
	if len(e.sender.sent) != 2 || !strings.Contains(e.sender.sent[1].Text, "Повторная") {
		t.Errorf("ожидалось второе уведомление с пометкой «Повторная»: %+v", e.sender.sent)
	}
}

func TestPublicLead_CORS(t *testing.T) {
	t.Run("preflight разрешённого origin", func(t *testing.T) {
		e := newPublicEnv(t)
		req := httptest.NewRequest(http.MethodOptions, "/api/public/lead", nil)
		req.Header.Set("Origin", publicTestOrigin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		w := httptest.NewRecorder()
		e.router.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("статус = %d, ожидался 204", w.Code)
		}
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != publicTestOrigin {
			t.Errorf("Allow-Origin = %q", got)
		}
		if got := w.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Content-Type") {
			t.Errorf("Allow-Headers = %q, ожидался Content-Type", got)
		}
		if len(e.limiter.keys) != 0 {
			t.Error("preflight не должен тратить бюджет rate limit")
		}
	})

	t.Run("чужой origin — 403", func(t *testing.T) {
		e := newPublicEnv(t)
		w := e.post(t, "https://evil.example", leadJSON(nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("статус = %d, ожидался 403", w.Code)
		}
		if !strings.Contains(w.Body.String(), codeOriginForbidden) {
			t.Errorf("тело без %s: %s", codeOriginForbidden, w.Body.String())
		}
		if len(e.leads.created) != 0 {
			t.Error("заявка с чужого origin не должна создавать лида")
		}
	})

	t.Run("без Origin (curl) — работает", func(t *testing.T) {
		e := newPublicEnv(t)
		if w := e.post(t, "", leadJSON(nil)); w.Code != http.StatusOK {
			t.Fatalf("статус = %d, ожидался 200", w.Code)
		}
	})
}

func TestPublicLead_RateLimit(t *testing.T) {
	t.Run("лимит исчерпан — 429", func(t *testing.T) {
		e := newPublicEnv(t)
		e.limiter.allow = false
		w := e.post(t, publicTestOrigin, leadJSON(nil))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("статус = %d, ожидался 429", w.Code)
		}
		if len(e.leads.created) != 0 || len(e.sender.sent) != 0 {
			t.Error("после 429 ничего создаваться и слаться не должно")
		}
		if len(e.limiter.keys) != 1 || !strings.HasPrefix(e.limiter.keys[0], "public:") {
			t.Errorf("ключ лимитера = %v, ожидался префикс public: (отдельный бюджет от /api)", e.limiter.keys)
		}
	})

	t.Run("лимитер упал — fail-open", func(t *testing.T) {
		e := newPublicEnv(t)
		e.limiter.allow = false
		e.limiter.err = errors.New("redis down")
		if w := e.post(t, publicTestOrigin, leadJSON(nil)); w.Code != http.StatusOK {
			t.Fatalf("статус = %d, ожидался 200 (fail-open, как §4.2)", w.Code)
		}
	})
}

// TestPublicLead_TelegramDown — уведомление менеджеру best effort: лид в БД,
// значит заявка принята (2xx), даже если Telegram отказал.
func TestPublicLead_TelegramDown(t *testing.T) {
	e := newPublicEnv(t)
	e.sender.err = errors.New("telegram: 502")
	w := e.post(t, publicTestOrigin, leadJSON(nil))
	if w.Code != http.StatusOK {
		t.Fatalf("статус = %d, ожидался 200: лид сохранён, уведомление best effort", w.Code)
	}
	if len(e.leads.created) != 1 || len(e.msgs.inbound) != 1 {
		t.Error("лид и сообщение должны сохраниться несмотря на отказ Telegram")
	}
}

// TestPublicLead_DBDown — Postgres лежит: последняя линия — заявка текстом в
// чат менеджера (200). Отказали оба — 500, сайт откатится на mailto.
func TestPublicLead_DBDown(t *testing.T) {
	t.Run("уведомление спасает заявку", func(t *testing.T) {
		e := newPublicEnv(t)
		e.leads.getErr = errors.New("pg down")
		w := e.post(t, publicTestOrigin, leadJSON(nil))
		if w.Code != http.StatusOK {
			t.Fatalf("статус = %d, ожидался 200 (заявка ушла менеджеру)", w.Code)
		}
		if len(e.sender.sent) != 1 || !strings.Contains(e.sender.sent[0].Text, "не сохранила") {
			t.Errorf("ожидалось уведомление с пометкой о несохранённой заявке: %+v", e.sender.sent)
		}
	})

	t.Run("отказали БД и Telegram — 500", func(t *testing.T) {
		e := newPublicEnv(t)
		e.leads.getErr = errors.New("pg down")
		e.sender.err = errors.New("telegram: 502")
		w := e.post(t, publicTestOrigin, leadJSON(nil))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("статус = %d, ожидался 500 (mailto-фоллбэк сайта)", w.Code)
		}
	})
}

// TestPublicLead_UnknownLang — язык вне ru/en/es (CHECK 0015) не пишется.
func TestPublicLead_UnknownLang(t *testing.T) {
	e := newPublicEnv(t)
	if w := e.post(t, publicTestOrigin, leadJSON(map[string]string{"lang": "pt"})); w.Code != http.StatusOK {
		t.Fatalf("статус = %d", w.Code)
	}
	if e.leads.created[0].Language != nil {
		t.Errorf("language = %q, ожидался NULL для неподдерживаемого языка", *e.leads.created[0].Language)
	}
}
