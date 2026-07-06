package telegram

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tele "gopkg.in/telebot.v3"

	"github.com/interfin/interfin-ai-crm/internal/config"
)

func testCfg() config.TelegramConfig {
	return config.TelegramConfig{
		BotToken:      "123:test-token",
		WebhookSecret: "hook-secret",
		WebhookURL:    "https://crm.example.org/webhook/telegram",
	}
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Критерий приёмки M2: «Polling нигде не используется» (CLAUDE.md §4.10).
func TestNewWebhookBot_WebhookPollerOnly(t *testing.T) {
	bot, err := NewWebhookBot(testCfg(), discardLog(), true)
	if err != nil {
		t.Fatalf("NewWebhookBot: %v", err)
	}

	wh, ok := bot.Poller.(*tele.Webhook)
	if !ok {
		t.Fatalf("poller = %T, обязан быть *telebot.Webhook (никакого polling)", bot.Poller)
	}
	if wh.Listen != "" {
		t.Errorf("Listen = %q: собственный listener запрещён, маршрут отдаёт gin (§6.1)", wh.Listen)
	}
	if wh.Endpoint == nil || wh.Endpoint.PublicURL != "https://crm.example.org/webhook/telegram" {
		t.Errorf("PublicURL не проброшен из конфига: %+v", wh.Endpoint)
	}
	if wh.SecretToken != "hook-secret" {
		t.Errorf("SecretToken не проброшен: Telegram не будет подписывать запросы (§5.4)")
	}
}

func TestNewWebhookBot_RequiresWebhookURL(t *testing.T) {
	cfg := testCfg()
	cfg.WebhookURL = ""
	if _, err := NewWebhookBot(cfg, discardLog(), true); err == nil {
		t.Fatal("без webhook_url бот создаваться не должен: setWebhook некуда делать")
	}
}

// Задача M2 §6: при старте вызывается setWebhook с URL и секретом.
func TestRegisterWebhook_CallsSetWebhook(t *testing.T) {
	var gotPath string
	var gotBody string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(body)
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer api.Close()

	bot, err := tele.NewBot(tele.Settings{
		Token:   "123:test-token",
		URL:     api.URL, // фейковый Telegram API
		Offline: true,
		Poller: &tele.Webhook{
			Listen:      "",
			SecretToken: "hook-secret",
			Endpoint:    &tele.WebhookEndpoint{PublicURL: "https://crm.example.org/webhook/telegram"},
		},
	})
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	if err := RegisterWebhook(bot); err != nil {
		t.Fatalf("RegisterWebhook: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/setWebhook") {
		t.Errorf("ожидали вызов setWebhook, был %q", gotPath)
	}
	if !strings.Contains(gotBody, "crm.example.org/webhook/telegram") {
		t.Errorf("setWebhook без public URL: %s", gotBody)
	}
	if !strings.Contains(gotBody, "hook-secret") {
		t.Errorf("setWebhook без secret_token — Telegram не будет подписывать запросы (§5.4): %s", gotBody)
	}
}

func TestRegisterWebhook_FailsOnAPIError(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"ok":false,"error_code":400,"description":"bad webhook url"}`))
	}))
	defer api.Close()

	bot, err := tele.NewBot(tele.Settings{
		Token:   "123:test-token",
		URL:     api.URL,
		Offline: true,
		Poller:  &tele.Webhook{Endpoint: &tele.WebhookEndpoint{PublicURL: "https://bad"}},
	})
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	if err := RegisterWebhook(bot); err == nil {
		t.Fatal("ошибка setWebhook должна быть фатальной, а не молчаливой")
	}
}

func TestRegisterWebhook_RejectsNonWebhookPoller(t *testing.T) {
	bot, err := tele.NewBot(tele.Settings{
		Token:   "123:test-token",
		Offline: true,
		Poller:  &tele.LongPoller{}, // так конфигурировать запрещено
	})
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	if err := RegisterWebhook(bot); err == nil {
		t.Fatal("RegisterWebhook обязан отвергать не-webhook poller (CLAUDE.md §4.10)")
	}
}

// Dispatcher доставляет апдейт в хендлеры бота (понадобится с M3+).
func TestDispatcher_DeliversUpdateToBotHandlers(t *testing.T) {
	// Synchronous — только чтобы тест не гонялся с goroutine хендлера;
	// prod-бот (NewWebhookBot) остаётся асинхронным.
	bot, err := tele.NewBot(tele.Settings{
		Token:       "123:test-token",
		Offline:     true,
		Synchronous: true,
		Poller:      &tele.Webhook{Endpoint: &tele.WebhookEndpoint{PublicURL: "https://x"}},
	})
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	var gotText string
	bot.Handle(tele.OnText, func(c tele.Context) error {
		gotText = c.Text()
		return nil
	})

	body := `{"update_id":1,"message":{"message_id":9,"from":{"id":7,"first_name":"A"},"chat":{"id":7,"type":"private"},"text":"привет"}}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/telegram", strings.NewReader(body))
	NewDispatcher(bot).ServeHTTP(httptest.NewRecorder(), req)

	if gotText != "привет" {
		t.Errorf("хендлер бота не получил апдейт: text=%q", gotText)
	}
}
