// EP-05: живой смоук контактов и сценария (критерий приёмки №1, ТЗ §9 п.6):
// вкладки 4–5 настраиваются через API панели → лид проходит четыре сцены:
//
//	A) /start → настроенное приветствие БЕЗ Claude, с клавиатурой кнопки;
//	B) прямой вопрос про сайт → ЖИВОЙ Claude даёт контакт из справочника;
//	C) «позовите живого человека» → Claude ставит {{handoff}}: текст клиенту
//	   без маркера, режим human, WS dialog_mode(client_handoff), уведомление
//	   в чат менеджеров, takeover:reminder взведён, emma_events handoff;
//	D) нажатие кнопки менеджера → то же с настроенным текстом подтверждения,
//	   без вызова Claude.
//
// Требует POSTGRES_TEST_DSN, REDIS_TEST_ADDR и ANTHROPIC_API_KEY — иначе
// skip. ВАЖНО: dev app-контейнер останавливать (общая очередь Redis —
// грабля EP-03 №1).
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	tele "gopkg.in/telebot.v3"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/claude"
	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/emma"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/handlers"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/repo"
	"github.com/interfin/interfin-ai-crm/internal/settings"
	"github.com/interfin/interfin-ai-crm/internal/telegram"
	"github.com/interfin/interfin-ai-crm/internal/worker"
)

const (
	// handoffLeadTgID — синтетический лид смоука (880001 M3, 880002 EP-03,
	// 880003 EP-04 заняты).
	handoffLeadTgID = int64(880004)
	// handoffManagerChat — «чат менеджеров» (мок ловит по chat_id).
	handoffManagerChat = int64(880999)

	smokeWelcome = "Здравствуйте! Я Эмма из «Свои в Бразилии» — расскажу об оформлении гражданства. Чем помочь?"
	smokeButton  = "Связаться с менеджером"
	smokeConfirm = "Сейчас свяжу вас с менеджером, ожидайте"
)

// wsCollector — подписка на crm:events (реальный Redis pub/sub): смоук
// проверяет, что карточка «подсветится» — событие dialog_mode дошло.
type wsCollector struct {
	mu  sync.Mutex
	evs []events.Event
}

func (c *wsCollector) run(ctx context.Context, rdb *redis.Client) {
	sub := rdb.Subscribe(ctx, events.Channel)
	ch := sub.Channel()
	go func() {
		defer sub.Close() //nolint:errcheck // завершение теста
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				var ev events.Event
				if err := json.Unmarshal([]byte(msg.Payload), &ev); err == nil {
					c.mu.Lock()
					c.evs = append(c.evs, ev)
					c.mu.Unlock()
				}
			}
		}
	}()
}

func (c *wsCollector) dialogMode(reason string) []events.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []events.Event
	for _, ev := range c.evs {
		if ev.Type == events.TypeDialogMode && ev.Reason == reason {
			out = append(out, ev)
		}
	}
	return out
}

func TestE2E_EmmaHandoffAndContacts(t *testing.T) {
	dsn := requireEnv(t, "POSTGRES_TEST_DSN")
	redisAddr := requireEnv(t, "REDIS_TEST_ADDR")
	anthropicKey := requireEnv(t, "ANTHROPIC_API_KEY")

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	cleanRedis(t, redisAddr)
	ctx := context.Background()

	// --- Postgres: зачистка следов ИМЕННО этого смоука ---
	gormDB, err := db.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 4, QueryExecMode: "simple"})
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	sqlDB, err := gormDB.DB()
	if err != nil {
		t.Fatalf("sql db: %v", err)
	}
	defer sqlDB.Close()
	for _, q := range []string{
		"DELETE FROM emma_events WHERE lead_id IN (SELECT id FROM leads WHERE telegram_user_id = " + fmt.Sprint(handoffLeadTgID) + ")",
		"DELETE FROM messages WHERE lead_id IN (SELECT id FROM leads WHERE telegram_user_id = " + fmt.Sprint(handoffLeadTgID) + ")",
		"DELETE FROM leads WHERE telegram_user_id = " + fmt.Sprint(handoffLeadTgID),
		// Весь справочник: чужой контакт «про сайт» Claude может выбрать
		// вместо нашего (прецедент EP-04 с библиотекой файлов).
		"DELETE FROM emma_contacts",
		// PIN и ключи вкладки 5: смоук настраивает всё с нуля через API.
		"DELETE FROM settings WHERE key IN ('" + strings.Join([]string{
			settings.KeyPinHash, settings.KeyWelcomeText, settings.KeyManagerButtonEnabled,
			settings.KeyManagerButtonText, settings.KeyHandoffConfirmText,
		}, "','") + "')",
	} {
		if err := gormDB.Exec(q).Error; err != nil {
			t.Fatalf("зачистка: %v", err)
		}
	}

	leads, msgs, _ := repo.New(gormDB)
	_, summaries, _ := repo.NewRAG(gormDB)
	contacts := repo.NewEmmaContacts(gormDB)
	emmaEvents := repo.NewEmmaEvents(gormDB)
	settingsSvc := settings.New(repo.NewSettings(gormDB))

	// --- «Telegram» (мок Bot API) ---
	mock := &telegramMock{}
	tgServer := httptest.NewServer(mock.handler(t))
	defer tgServer.Close()
	bot, err := tele.NewBot(tele.Settings{
		Token:   "42:TEST-TOKEN",
		URL:     tgServer.URL,
		Offline: true,
		Poller:  &tele.Webhook{Listen: "", Endpoint: &tele.WebhookEndpoint{PublicURL: "https://example.org/webhook/telegram"}},
	})
	if err != nil {
		t.Fatalf("telebot: %v", err)
	}

	// --- Живой Claude (боевой конфиг §7.2, system 5000 EP-02) ---
	claudeCfg := config.ClaudeConfig{
		APIKey:            anthropicKey,
		Model:             "claude-sonnet-5",
		ClaudeReplyTokens: 1000,
		TokenBudget: config.TokenBudget{
			SystemPrompt: 5000, Summary: 1000, History: 4000, SafetyBuffer: 1000,
		},
		CountTokensThreshold: 7500,
	}
	ai, err := claude.New(claudeCfg)
	if err != nil {
		t.Fatalf("claude: %v", err)
	}
	budgeter, err := worker.NewBudgeter(claudeCfg, ai)
	if err != nil {
		t.Fatalf("budgeter: %v", err)
	}

	// --- Очередь, WS-коллектор, воркер (боевая сборка EP-05) ---
	redisCfg := config.RedisConfig{Addr: redisAddr}
	q := queue.NewClient(redisCfg)
	defer q.Close()
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()
	pub := events.NewRedisPublisher(rdb)
	sender := worker.NewTelebotSender(bot)

	wsCtx, wsCancel := context.WithCancel(ctx)
	defer wsCancel()
	ws := &wsCollector{}
	ws.run(wsCtx, rdb)

	wrk := worker.New(redisCfg,
		worker.NewProcessor(worker.ProcessorDeps{
			Leads:         leads,
			Msgs:          msgs,
			Budgeter:      budgeter,
			AI:            ai,
			Sender:        sender,
			Summaries:     summaries,
			SummaryEnq:    q,
			SummaryEveryN: 15,
			Pub:           pub,
			Settings:      settingsSvc,
			TakeoverEnq:   q,
			Prompt:        worker.NewPromptProvider(repo.NewEmmaPrompts(gormDB), log),
			Events:        emmaEvents,
			// EP-05: секция контактов, ключи вкладки 5, чат менеджеров.
			Contacts:      worker.NewContactsProvider(contacts, log),
			Panel:         settingsSvc,
			ManagerChatID: handoffManagerChat,
			Log:           log,
		}),
		worker.NewSummarizer(leads, msgs, summaries, ai, worker.NewRedisLocker(rdb), log),
		sender, 0, log)
	if err := wrk.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer wrk.Shutdown()

	// --- HTTP: вебхук M2 + панель Эммы (цепочка как в cmd/server) ---
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers.NewTelegramWebhook(leads, msgs, q, pub, webhookSecret, log).
		Register(router, telegram.NewDispatcher(bot))

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	api := router.Group("/api")
	api.Use(
		auth.Middleware(auth.NewVerifier(&rsaKey.PublicKey)),
		auth.RequireRole(auth.RoleManager, auth.RoleAdmin),
	)
	pinStore := emma.NewRedisStore(rdb)
	emmaGroup := api.Group("/emma", auth.RequireRole(auth.RoleAdmin), emma.Audit(log))
	emma.NewPIN(emma.PINDeps{Settings: settingsSvc, Store: pinStore, Log: log}).
		Register(emmaGroup.Group("/pin"))
	protected := emmaGroup.Group("", emma.RequirePIN(pinStore, log))
	emma.NewContacts(emma.ContactsDeps{Contacts: contacts, Log: log}).Register(protected)
	emma.NewScenario(emma.ScenarioDeps{Settings: settingsSvc, Log: log}).Register(protected)

	adminToken, err := auth.NewIssuer(rsaKey, time.Minute).Issue("1", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	doJSON := func(method, path string, body any) *httptest.ResponseRecorder {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	sendUpdate := func(updateID, msgID int, text string) {
		t.Helper()
		update := fmt.Sprintf(`{
			"update_id": %d,
			"message": {
				"message_id": %d,
				"date": %d,
				"text": %q,
				"from": {"id": %d, "is_bot": false, "first_name": "Смоук", "username": "e2e_handoff_lead"},
				"chat": {"id": %d, "type": "private"}
			}
		}`, updateID, msgID, time.Now().Unix(), text, handoffLeadTgID, handoffLeadTgID)
		req := httptest.NewRequest(http.MethodPost, "/webhook/telegram", bytes.NewBufferString(update))
		req.Header.Set(handlers.SecretTokenHeader, webhookSecret)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("webhook (%q): %d %s", text, rec.Code, rec.Body.String())
		}
	}
	// leadSent — сообщения, ушедшие ЛИДУ (не в чат менеджеров).
	leadSent := func() []sentMessage {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		var out []sentMessage
		for _, s := range mock.sent {
			if s.ChatID == fmt.Sprint(handoffLeadTgID) {
				out = append(out, s)
			}
		}
		return out
	}
	managerSent := func() []sentMessage {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		var out []sentMessage
		for _, s := range mock.sent {
			if s.ChatID == fmt.Sprint(handoffManagerChat) {
				out = append(out, s)
			}
		}
		return out
	}
	waitFor := func(what string, timeout time.Duration, done func() bool) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for !done() && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
		if !done() {
			t.Fatalf("не дождались: %s", what)
		}
	}

	// --- Настройка панели: PIN → вкладка 5 → контакт вкладки 4 ---
	if w := doJSON(http.MethodPost, "/api/emma/pin/setup", gin.H{"pin": "123456"}); w.Code != http.StatusOK {
		t.Fatalf("pin/setup: %d %s", w.Code, w.Body.String())
	}
	w := doJSON(http.MethodPatch, "/api/emma/scenario", gin.H{
		"welcome_text":           smokeWelcome,
		"manager_button_enabled": true,
		"manager_button_text":    smokeButton,
		"handoff_confirm_text":   smokeConfirm,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("scenario patch: %d %s", w.Code, w.Body.String())
	}
	w = doJSON(http.MethodPost, "/api/emma/contacts", gin.H{
		"type": "website", "name": "Сайт сервиса", "value": "svoibrazil.ru",
		"comment": "давай, когда клиент спрашивает сайт или хочет почитать подробнее",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("contact create: %d %s", w.Code, w.Body.String())
	}

	// --- Сцена A: /start → welcome без Claude, с клавиатурой ---
	sendUpdate(11, 801, "/start")
	waitFor("welcome на /start", 30*time.Second, func() bool { return len(leadSent()) >= 1 })
	welcome := leadSent()[0]
	t.Logf("A. /start → %q, markup: %s", welcome.Text, welcome.ReplyMarkup)
	if welcome.Text != smokeWelcome {
		t.Errorf("A. welcome: %q, ждали настроенный текст (Claude вызываться не должен)", welcome.Text)
	}
	if !strings.Contains(welcome.ReplyMarkup, smokeButton) ||
		!strings.Contains(welcome.ReplyMarkup, `"resize_keyboard":true`) ||
		!strings.Contains(welcome.ReplyMarkup, `"is_persistent":true`) {
		t.Errorf("A. клавиатура не приложена к welcome: %q", welcome.ReplyMarkup)
	}

	// --- Сцена B: прямой вопрос про сайт → контакт из справочника ---
	sendUpdate(12, 802, "Подскажите, какой у вас сайт? Хочу почитать подробнее.")
	waitFor("ответ про сайт (живой Claude)", 90*time.Second, func() bool { return len(leadSent()) >= 2 })
	site := leadSent()[1]
	t.Logf("B. про сайт → %q", site.Text)
	if !strings.Contains(strings.ToLower(site.Text), "svoibrazil.ru") {
		t.Errorf("B. Эмма не дала контакт из справочника: %q", site.Text)
	}
	if !strings.Contains(site.ReplyMarkup, smokeButton) {
		t.Errorf("B. ответ Эммы без клавиатуры кнопки: %q", site.ReplyMarkup)
	}

	// --- Сцена C: просьба фразой → {{handoff}} от живого Claude ---
	sendUpdate(13, 803, "Позовите, пожалуйста, живого человека — хочу поговорить с менеджером.")
	waitFor("handoff по фразе (ответ + режим human)", 90*time.Second, func() bool {
		if len(leadSent()) < 3 {
			return false
		}
		lead, err := leads.GetByTelegramUserID(ctx, handoffLeadTgID)
		return err == nil && lead.DialogMode == models.DialogModeHuman
	})
	phrase := leadSent()[2]
	t.Logf("C. фраза → %q", phrase.Text)
	if strings.Contains(phrase.Text, "{{") || strings.Contains(phrase.Text, "}}") {
		t.Errorf("C. маркер утёк клиенту: %q", phrase.Text)
	}
	// Confirm-текст НЕ дублируется вторым сообщением (ТЗ §4 п.2).
	if got := leadSent(); len(got) != 3 {
		t.Errorf("C. лиду ушло %d сообщений, ждали 3 (без дубля confirm): %+v", len(got), got)
	}
	waitFor("уведомление менеджерам о фразе", 10*time.Second, func() bool { return len(managerSent()) >= 1 })
	notice := managerSent()[0]
	t.Logf("C. уведомление менеджерам → %q", notice.Text)
	if !strings.Contains(notice.Text, "просит менеджера") || !strings.Contains(notice.Text, "Смоук") {
		t.Errorf("C. текст уведомления: %q", notice.Text)
	}
	waitFor("WS dialog_mode(client_handoff)", 10*time.Second, func() bool {
		return len(ws.dialogMode(events.ReasonClientHandoff)) >= 1
	})
	dm := ws.dialogMode(events.ReasonClientHandoff)[0]
	if dm.Mode != models.DialogModeHuman || dm.TakenBy != nil {
		t.Errorf("C. событие dialog_mode: %+v", dm)
	}

	// takeover:reminder взведён (задача в scheduled очереди Asynq).
	insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: redisAddr})
	defer insp.Close()
	scheduled, err := insp.ListScheduledTasks("default", asynq.PageSize(50))
	if err != nil {
		t.Fatalf("inspector: %v", err)
	}
	reminders := 0
	for _, task := range scheduled {
		if task.Type == queue.TypeTakeoverReminder {
			reminders++
		}
	}
	if reminders != 1 {
		t.Errorf("C. takeover:reminder в scheduled: %d, ждали 1", reminders)
	}

	// --- Сцена D: кнопка менеджера (после возврата диалога Эмме) ---
	lead, err := leads.GetByTelegramUserID(ctx, handoffLeadTgID)
	if err != nil {
		t.Fatal(err)
	}
	// «Менеджер вернул Эмме» (PATCH /mode из M13 — здесь напрямую в БД).
	if err := leads.UpdateFields(ctx, lead.ID, map[string]interface{}{
		"dialog_mode": models.DialogModeBot, "bot_silenced_until": nil, "taken_by": nil,
	}); err != nil {
		t.Fatal(err)
	}

	sendUpdate(14, 804, smokeButton)
	waitFor("handoff по кнопке (confirm + режим human)", 30*time.Second, func() bool {
		if len(leadSent()) < 4 {
			return false
		}
		fresh, err := leads.GetByTelegramUserID(ctx, handoffLeadTgID)
		return err == nil && fresh.DialogMode == models.DialogModeHuman
	})
	confirm := leadSent()[3]
	t.Logf("D. кнопка → %q", confirm.Text)
	if confirm.Text != smokeConfirm {
		t.Errorf("D. confirm: %q, ждали настроенный текст (Claude вызываться не должен)", confirm.Text)
	}
	if !strings.Contains(confirm.ReplyMarkup, smokeButton) {
		t.Errorf("D. confirm без клавиатуры: %q", confirm.ReplyMarkup)
	}
	waitFor("второе уведомление менеджерам", 10*time.Second, func() bool { return len(managerSent()) >= 2 })
	if got := ws.dialogMode(events.ReasonClientHandoff); len(got) < 2 {
		t.Errorf("D. второе событие dialog_mode(client_handoff) не пришло: %+v", got)
	}

	// --- Журнал: два события handoff (фраза + кнопка) ---
	var handoffCount int64
	if err := gormDB.Model(&models.EmmaEvent{}).
		Where("event_type = ? AND lead_id = ?", models.EmmaEventHandoff, lead.ID).
		Count(&handoffCount).Error; err != nil {
		t.Fatal(err)
	}
	if handoffCount != 2 {
		t.Errorf("emma_events handoff: %d записей, ждали 2", handoffCount)
	}

	// Маркеров в истории нет; welcome и confirm сохранены как outbound.
	history, err := msgs.ListByLead(ctx, lead.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var haveWelcome, haveConfirm bool
	for _, m := range history {
		if strings.Contains(m.Content, "{{") {
			t.Errorf("маркер утёк в messages: %q", m.Content)
		}
		if m.Direction == models.DirectionOutbound && m.Content == smokeWelcome {
			haveWelcome = true
		}
		if m.Direction == models.DirectionOutbound && m.Content == smokeConfirm {
			haveConfirm = true
		}
	}
	if !haveWelcome || !haveConfirm {
		t.Errorf("в messages нет welcome (%v) или confirm (%v); история: %d строк",
			haveWelcome, haveConfirm, len(history))
	}
}
