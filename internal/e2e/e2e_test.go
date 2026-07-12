// Package e2e — сквозная проверка критерия приёмки M3 №1:
// «Лид пишет в Telegram → получает осмысленный ответ Claude».
//
// Поднимается ВЕСЬ боевой контур в одном процессе: gin-цепочка webhook (M2),
// реальные Postgres и Redis, Asynq-воркер и ЖИВОЙ Anthropic API. Единственная
// подмена — Telegram Bot API: telebot направляется в локальный httptest-сервер
// (публичного URL для настоящего вебхука в тестовой среде нет).
//
// Запуск требует трёх переменных, иначе skip:
//   - POSTGRES_TEST_DSN (схема из migrations/ уже применена — как в repo-тестах);
//   - REDIS_TEST_ADDR;
//   - ANTHROPIC_API_KEY (живой вызов Claude; в CI не задан → тест скипается).
package e2e

import (
	"bytes"
	"context"
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
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	tele "gopkg.in/telebot.v3"

	"github.com/interfin/interfin-ai-crm/internal/claude"
	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/handlers"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/repo"
	"github.com/interfin/interfin-ai-crm/internal/telegram"
	"github.com/interfin/interfin-ai-crm/internal/worker"
)

const (
	webhookSecret = "e2e-secret"
	// leadTgID — telegram_user_id синтетического лида (не пересекается
	// с данными других интеграционных тестов).
	leadTgID = int64(880001)
)

// sentMessage — что «Telegram» получил в sendMessage.
type sentMessage struct {
	ChatID string
	Text   string
}

// sentTgFile — доставка sendDocument/sendPhoto (EP-04): telebot шлёт файлы
// multipart'ом — файл в части document/photo, chat_id form-значением.
type sentTgFile struct {
	ChatID   string
	Method   string // sendDocument | sendPhoto
	FileName string
	Size     int64
}

// telegramMock — минимальный Telegram Bot API для telebot: sendChatAction и
// sendMessage (JSON), sendDocument/sendPhoto (multipart, EP-04). Остальные
// методы не ожидаются.
type telegramMock struct {
	mu      sync.Mutex
	actions int
	sent    []sentMessage
	files   []sentTgFile
}

func (m *telegramMock) handler(t *testing.T) http.HandlerFunc {
	decodeJSON := func(r *http.Request) map[string]interface{} {
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("telegram mock: тело %s не разобрано: %v", r.URL.Path, err)
		}
		return payload
	}
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
			decodeJSON(r)
			m.actions++
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			payload := decodeJSON(r)
			m.sent = append(m.sent, sentMessage{
				ChatID: fmt.Sprint(payload["chat_id"]),
				Text:   fmt.Sprint(payload["text"]),
			})
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":880001,"type":"private"}}}`)
		case strings.HasSuffix(r.URL.Path, "/sendDocument"), strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			method, field := "sendDocument", "document"
			if strings.HasSuffix(r.URL.Path, "/sendPhoto") {
				method, field = "sendPhoto", "photo"
			}
			if err := r.ParseMultipartForm(64 << 20); err != nil {
				t.Errorf("telegram mock: multipart %s не разобран: %v", r.URL.Path, err)
			}
			f := sentTgFile{ChatID: r.FormValue("chat_id"), Method: method}
			if r.MultipartForm != nil {
				if fhs := r.MultipartForm.File[field]; len(fhs) > 0 {
					f.FileName = fhs[0].Filename
					f.Size = fhs[0].Size
				}
			}
			m.files = append(m.files, f)
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":2,"date":1,"chat":{"id":880001,"type":"private"}}}`)
		default:
			t.Errorf("telegram mock: неожиданный метод %s", r.URL.Path)
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	}
}

func (m *telegramMock) fileCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.files)
}

func (m *telegramMock) lastFile() sentTgFile {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.files[len(m.files)-1]
}

func (m *telegramMock) sentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func (m *telegramMock) lastSent() sentMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sent[len(m.sent)-1]
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s не задан — e2e-тест пропущен", name)
	}
	return v
}

// cleanRedis прибирает следы прошлых прогонов: все состояния очереди
// default и unique-замки asynq (DeleteTask их не снимает).
func cleanRedis(t *testing.T, addr string) {
	t.Helper()
	insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: addr})
	defer insp.Close()
	for _, fn := range []func(string) (int, error){
		insp.DeleteAllPendingTasks,
		insp.DeleteAllScheduledTasks,
		insp.DeleteAllRetryTasks,
		insp.DeleteAllArchivedTasks,
		insp.DeleteAllCompletedTasks,
	} {
		if _, err := fn("default"); err != nil && err != asynq.ErrQueueNotFound {
			t.Logf("очистка очереди: %v", err)
		}
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	ctx := context.Background()
	if keys, err := rdb.Keys(ctx, "asynq:{default}:unique:*").Result(); err == nil && len(keys) > 0 {
		if err := rdb.Del(ctx, keys...).Err(); err != nil {
			t.Logf("зачистка unique-замков: %v", err)
		}
	}
}

// TestE2E_LeadWritesAndGetsClaudeReply — критерий приёмки M3 №1.
func TestE2E_LeadWritesAndGetsClaudeReply(t *testing.T) {
	dsn := requireEnv(t, "POSTGRES_TEST_DSN")
	redisAddr := requireEnv(t, "REDIS_TEST_ADDR")
	apiKey := requireEnv(t, "ANTHROPIC_API_KEY")

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	cleanRedis(t, redisAddr)

	// --- Postgres (реальный, схема M1) ---
	gormDB, err := db.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 4, QueryExecMode: "simple"})
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	sqlDB, err := gormDB.DB()
	if err != nil {
		t.Fatalf("sql db: %v", err)
	}
	defer sqlDB.Close()
	// Следы прошлых прогонов этого лида (FK messages→leads).
	if err := gormDB.Exec(
		"DELETE FROM messages WHERE lead_id IN (SELECT id FROM leads WHERE telegram_user_id = ?)", leadTgID).Error; err != nil {
		t.Fatalf("очистка messages: %v", err)
	}
	if err := gormDB.Exec("DELETE FROM leads WHERE telegram_user_id = ?", leadTgID).Error; err != nil {
		t.Fatalf("очистка leads: %v", err)
	}

	leads, msgs, _ := repo.New(gormDB)
	_, summaries, _ := repo.NewRAG(gormDB)

	// --- «Telegram»: локальный мок Bot API ---
	mock := &telegramMock{}
	tgServer := httptest.NewServer(mock.handler(t))
	defer tgServer.Close()

	bot, err := tele.NewBot(tele.Settings{
		Token:   "42:TEST-TOKEN",
		URL:     tgServer.URL, // telebot ходит в мок вместо api.telegram.org
		Offline: true,         // без getMe при старте
		Poller:  &tele.Webhook{Listen: "", Endpoint: &tele.WebhookEndpoint{PublicURL: "https://example.org/webhook/telegram"}},
	})
	if err != nil {
		t.Fatalf("telebot: %v", err)
	}

	// --- Claude: ЖИВОЙ API, боевой конфиг §7.2 (system 5000 — EP-02) ---
	claudeCfg := config.ClaudeConfig{
		APIKey:            apiKey,
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

	// --- Очередь + воркер (боевая сборка, как в cmd/server) ---
	redisCfg := config.RedisConfig{Addr: redisAddr}
	q := queue.NewClient(redisCfg)
	defer q.Close()

	sender := worker.NewTelebotSender(bot)
	// Retriever не задан: e2e закрывает критерий M3 (диалог), база знаний
	// в тестовой БД пуста; RAG-контур закрывают тесты M4 (rag/repo/worker).
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()
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
			// EP-02: промпт из БД, как в боевой сборке cmd/server (сид 0021
			// уже в схеме; пустая таблица легально падает на константу).
			Prompt: worker.NewPromptProvider(repo.NewEmmaPrompts(gormDB), log),
			Log:    log,
		}),
		worker.NewSummarizer(leads, msgs, summaries, ai, worker.NewRedisLocker(rdb), log),
		sender, 0, log)
	if err := wrk.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer wrk.Shutdown()

	// --- HTTP-цепочка M2: POST /webhook/telegram ---
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers.NewTelegramWebhook(leads, msgs, q, events.NewRedisPublisher(rdb), webhookSecret, log).
		Register(router, telegram.NewDispatcher(bot))

	update := fmt.Sprintf(`{
		"update_id": 1,
		"message": {
			"message_id": 777,
			"date": %d,
			"text": "Здравствуйте! Расскажите, пожалуйста, чем занимается ваша компания?",
			"from": {"id": %d, "is_bot": false, "first_name": "Тест", "username": "e2e_lead"},
			"chat": {"id": %d, "type": "private"}
		}
	}`, time.Now().Unix(), leadTgID, leadTgID)

	req := httptest.NewRequest(http.MethodPost, "/webhook/telegram", bytes.NewBufferString(update))
	req.Header.Set(handlers.SecretTokenHeader, webhookSecret)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	// Telegram обязан получить 200 немедленно (CLAUDE.md §4.4).
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook ответил %d, ожидали 200; тело: %s", rec.Code, rec.Body.String())
	}

	// Ждём, пока воркер сходит в живой Claude и «отправит» ответ (latency 3–15с).
	deadline := time.Now().Add(90 * time.Second)
	for mock.sentCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if mock.sentCount() == 0 {
		t.Fatal("ответ лиду не отправлен за 90с (dead letter? смотри лог воркера)")
	}

	reply := mock.lastSent()
	t.Logf("ответ Claude лиду: %q", reply.Text)

	// Ответ ушёл в чат лида.
	if reply.ChatID != fmt.Sprint(leadTgID) {
		t.Errorf("ответ ушёл в чат %s, ожидали %d", reply.ChatID, leadTgID)
	}
	// «Осмысленный»: непустой, развёрнутый, на русском (системный промпт).
	if len([]rune(reply.Text)) < 20 {
		t.Errorf("ответ подозрительно короткий: %q", reply.Text)
	}
	if !strings.ContainsFunc(reply.Text, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }) {
		t.Errorf("ответ не на русском: %q", reply.Text)
	}
	// Индикатор «печатает…» был показан (§6.2 шаг 1).
	if mock.actions == 0 {
		t.Error("sendChatAction (typing) не вызывался")
	}

	// Ответ сохранён в messages как outbound ДО отправки (§6.2 шаги 4–5).
	ctx := context.Background()
	lead, err := leads.GetByTelegramUserID(ctx, leadTgID)
	if err != nil {
		t.Fatalf("лид не создан: %v", err)
	}
	history, err := msgs.ListByLead(ctx, lead.ID, 10)
	if err != nil {
		t.Fatalf("история: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("в messages %d строк, ожидали 2 (inbound + outbound)", len(history))
	}
	if history[0].Direction != "inbound" || history[1].Direction != "outbound" {
		t.Errorf("направления: %s, %s", history[0].Direction, history[1].Direction)
	}
	if history[1].Content != reply.Text {
		t.Error("сохранённый outbound не совпадает с отправленным лиду")
	}
	// Счётчик считает только inbound (CLAUDE.md §4.3).
	if lead.MessageCount != 1 {
		t.Errorf("message_count = %d, ожидали 1 (outbound не считается)", lead.MessageCount)
	}
}
