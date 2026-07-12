// EP-04: живой смоук файлов для отправки (критерий приёмки №1, ТЗ §9 п.5):
// PDF с описанием «отправь, когда клиент спрашивает цены» загружается через
// API панели (JWT admin + PIN) → лид спрашивает «сколько стоит?» через
// вебхук → ЖИВОЙ Claude видит секцию файлов, ставит маркер {{file:N}} →
// воркер шлёт текст БЕЗ маркера + sendDocument; в messages остаётся след
// [файл: …], в emma_events — file_sent.
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
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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
	// filesLeadTgID — синтетический лид смоука (не пересекается с 880001 M3
	// и 880002 EP-03).
	filesLeadTgID = int64(880003)
	// filesE2EName — название файла в библиотеке (уникально для зачистки).
	filesE2EName = "Прайс e2e EP-04"
)

// filesE2EPDF — минимальный валидный PDF (сигнатура %PDF — ручка проверяет).
var filesE2EPDF = []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n")

func TestE2E_EmmaSendsFileOnPriceQuestion(t *testing.T) {
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
		"DELETE FROM emma_events WHERE lead_id IN (SELECT id FROM leads WHERE telegram_user_id = " + fmt.Sprint(filesLeadTgID) + ")",
		"DELETE FROM messages WHERE lead_id IN (SELECT id FROM leads WHERE telegram_user_id = " + fmt.Sprint(filesLeadTgID) + ")",
		"DELETE FROM leads WHERE telegram_user_id = " + fmt.Sprint(filesLeadTgID),
		// ВСЯ библиотека, не только строка прошлого прогона: другой активный
		// файл с описанием про цены Claude может выбрать вместо нашего
		// (поймано первым прогоном — взял «Прайс 2026» живой dev-панели,
		// путь внутри контейнера). События отвяжутся сами (SET NULL);
		// dev-библиотека — расходный материал тестов (прецедент EP-02).
		"DELETE FROM emma_send_files",
		// PIN панели: смоук всегда проходит bootstrap с нуля.
		"DELETE FROM settings WHERE key = '" + settings.KeyPinHash + "'",
	} {
		if err := gormDB.Exec(q).Error; err != nil {
			t.Fatalf("зачистка: %v", err)
		}
	}

	leads, msgs, _ := repo.New(gormDB)
	_, summaries, _ := repo.NewRAG(gormDB)
	sendFiles := repo.NewEmmaSendFiles(gormDB)
	emmaEvents := repo.NewEmmaEvents(gormDB)
	settingsSvc := settings.New(repo.NewSettings(gormDB))

	// --- «Telegram» (мок Bot API: sendMessage + sendDocument) ---
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

	// --- Живой Claude ---
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

	// --- Очередь + воркер: боевая сборка EP-04 (провайдер секции, файлы,
	// журнал), RAG не нужен — вопрос о ценах закрывает библиотека файлов ---
	redisCfg := config.RedisConfig{Addr: redisAddr}
	q := queue.NewClient(redisCfg)
	defer q.Close()
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()
	pub := events.NewRedisPublisher(rdb)
	sender := worker.NewTelebotSender(bot)

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
			Prompt:        worker.NewPromptProvider(repo.NewEmmaPrompts(gormDB), log),
			// EP-04: секция файлов + валидация маркеров + журнал.
			FilesProv: worker.NewSendFilesProvider(sendFiles, log),
			SendFiles: sendFiles,
			Events:    emmaEvents,
			Log:       log,
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
	emma.NewFiles(emma.FilesDeps{Files: sendFiles, Dir: t.TempDir(), Log: log}).
		Register(protected)

	adminToken, err := auth.NewIssuer(rsaKey, time.Minute).Issue("1", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	do := func(req *http.Request) *httptest.ResponseRecorder {
		req.Header.Set("Authorization", "Bearer "+adminToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// --- Шаг 1: bootstrap PIN (сессия открывается сразу) ---
	pinBody := bytes.NewBufferString(`{"pin":"123456"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/emma/pin/setup", pinBody)
	req.Header.Set("Content-Type", "application/json")
	if w := do(req); w.Code != http.StatusOK {
		t.Fatalf("pin/setup: %d %s", w.Code, w.Body.String())
	}

	// --- Шаг 2: PDF с описанием-подсказкой через API панели ---
	var mpBody bytes.Buffer
	mw := multipart.NewWriter(&mpBody)
	if err := mw.WriteField("name", filesE2EName); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("description",
		"отправь, когда клиент спрашивает цены"); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("file", "прайс.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(filesE2EPDF); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/emma/files", &mpBody)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := do(req)
	if w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var uploaded struct {
		ID       int64 `json:"id"`
		IsActive bool  `json:"is_active"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	if !uploaded.IsActive {
		t.Fatalf("файл не активен после загрузки: %s", w.Body.String())
	}
	t.Logf("файл загружен: id=%d", uploaded.ID)

	// --- Шаг 3: лид спрашивает про цены через вебхук ---
	update := fmt.Sprintf(`{
		"update_id": 3,
		"message": {
			"message_id": 779,
			"date": %d,
			"text": "Здравствуйте! Сколько стоит ваше сопровождение? Пришлите, пожалуйста, подробные цены.",
			"from": {"id": %d, "is_bot": false, "first_name": "Смоук", "username": "e2e_files_lead"},
			"chat": {"id": %d, "type": "private"}
		}
	}`, time.Now().Unix(), filesLeadTgID, filesLeadTgID)
	req = httptest.NewRequest(http.MethodPost, "/webhook/telegram", bytes.NewBufferString(update))
	req.Header.Set(handlers.SecretTokenHeader, webhookSecret)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook: %d %s", rec.Code, rec.Body.String())
	}

	// --- Шаг 4: ждём текст + документ (живой Claude, 3–15 с на ответ) ---
	deadline := time.Now().Add(90 * time.Second)
	for (mock.sentCount() == 0 || mock.fileCount() == 0) && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if mock.sentCount() == 0 {
		t.Fatal("текстовый ответ лиду не отправлен за 90с")
	}
	reply := mock.lastSent()
	t.Logf("ответ Эммы: %q", reply.Text)
	// Маркеров в тексте клиента быть не должно (они вырезаются до Send).
	if strings.Contains(reply.Text, "{{") || strings.Contains(reply.Text, "}}") {
		t.Fatalf("маркер утёк клиенту: %q", reply.Text)
	}
	if mock.fileCount() == 0 {
		t.Fatal("Эмма не отправила файл на вопрос о ценах " +
			"(sendDocument не вызван — маркер не поставлен или файл отсеян)")
	}
	doc := mock.lastFile()
	t.Logf("доставлен файл: %+v", doc)
	if doc.Method != "sendDocument" {
		t.Errorf("PDF ушёл методом %s, ждали sendDocument", doc.Method)
	}
	if doc.FileName != filesE2EName+".pdf" {
		t.Errorf("имя документа %q, ждали %q", doc.FileName, filesE2EName+".pdf")
	}
	if doc.Size != int64(len(filesE2EPDF)) {
		t.Errorf("размер документа %d, ждали %d", doc.Size, len(filesE2EPDF))
	}
	if doc.ChatID != fmt.Sprint(filesLeadTgID) {
		t.Errorf("chat_id документа %s, ждали %d", doc.ChatID, filesLeadTgID)
	}

	// --- Шаг 5: след в чате менеджера и журнал событий ---
	lead, err := leads.GetByTelegramUserID(ctx, filesLeadTgID)
	if err != nil {
		t.Fatalf("лид смоука: %v", err)
	}
	history, err := msgs.ListByLead(ctx, lead.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantNote := "[файл: " + filesE2EName + "]"
	var haveNote bool
	for _, m := range history {
		if strings.Contains(m.Content, "{{") {
			t.Errorf("маркер утёк в messages: %q", m.Content)
		}
		if m.Content == wantNote && m.Direction == models.DirectionOutbound &&
			m.Author != nil && *m.Author == models.AuthorBot {
			haveNote = true
		}
	}
	if !haveNote {
		t.Errorf("в messages нет следа %q; история: %+v", wantNote, history)
	}

	var evCount int64
	if err := gormDB.Model(&models.EmmaEvent{}).
		Where("event_type = ? AND send_file_id = ? AND lead_id = ?",
			models.EmmaEventFileSent, uploaded.ID, lead.ID).
		Count(&evCount).Error; err != nil {
		t.Fatal(err)
	}
	if evCount != 1 {
		t.Errorf("emma_events file_sent: %d записей, ждали 1", evCount)
	}
}
