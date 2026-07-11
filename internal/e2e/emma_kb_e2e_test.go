// EP-03: живой смоук базы знаний панели (критерий приёмки №1, контур ТЗ §9
// п.4): TXT с уникальным фактом загружается через API панели (JWT admin +
// PIN) → asynq-воркер извлекает текст и индексирует ЖИВЫМ Voyage в pgvector
// → лид спрашивает Эмму через вебхук → ЖИВОЙ Claude отвечает фактом,
// которого нет нигде, кроме загруженного файла.
//
// Требует POSTGRES_TEST_DSN, REDIS_TEST_ADDR, ANTHROPIC_API_KEY и
// VOYAGE_API_KEY — иначе skip. Voyage free tier — 3 RPM: тест делает два
// вызова (индексация + запрос), укладывается.
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
	"github.com/interfin/interfin-ai-crm/internal/embeddings"
	"github.com/interfin/interfin-ai-crm/internal/emma"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/handlers"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/rag"
	"github.com/interfin/interfin-ai-crm/internal/repo"
	"github.com/interfin/interfin-ai-crm/internal/settings"
	"github.com/interfin/interfin-ai-crm/internal/telegram"
	"github.com/interfin/interfin-ai-crm/internal/worker"
)

const (
	// kbLeadTgID — синтетический лид смоука (не пересекается с 880001 M3).
	kbLeadTgID = int64(880002)
	// kbFactCode/kbFilename — уникальный факт, который существует ТОЛЬКО в файле.
	kbFactCode = "774411"
	kbFilename = "e2e-partner-code.txt"
)

func TestE2E_EmmaAnswersFromUploadedKB(t *testing.T) {
	dsn := requireEnv(t, "POSTGRES_TEST_DSN")
	redisAddr := requireEnv(t, "REDIS_TEST_ADDR")
	anthropicKey := requireEnv(t, "ANTHROPIC_API_KEY")
	voyageKey := requireEnv(t, "VOYAGE_API_KEY")

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
		"DELETE FROM messages WHERE lead_id IN (SELECT id FROM leads WHERE telegram_user_id = " + fmt.Sprint(kbLeadTgID) + ")",
		"DELETE FROM leads WHERE telegram_user_id = " + fmt.Sprint(kbLeadTgID),
		// Чанки прошлого прогона (source panel:<id> прежней строки) и сама строка.
		`DELETE FROM knowledge_chunks WHERE source IN
		   (SELECT 'panel:' || id FROM emma_kb_files WHERE filename = '` + kbFilename + `')`,
		"DELETE FROM emma_kb_files WHERE filename = '" + kbFilename + "'",
		// PIN панели: смоук всегда проходит bootstrap с нуля.
		"DELETE FROM settings WHERE key = '" + settings.KeyPinHash + "'",
	} {
		if err := gormDB.Exec(q).Error; err != nil {
			t.Fatalf("зачистка: %v", err)
		}
	}

	leads, msgs, _ := repo.New(gormDB)
	knowledge, summaries, ragAudit := repo.NewRAG(gormDB)
	emmaKB := repo.NewEmmaKB(gormDB)
	settingsSvc := settings.New(repo.NewSettings(gormDB))

	// --- «Telegram» (мок Bot API, как в смоуке M3) ---
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

	// --- Живые Claude и Voyage ---
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
	embedder, err := embeddings.New(config.EmbeddingsConfig{
		Provider: "voyage", APIKey: voyageKey, Model: "voyage-3", Dimensions: 1024,
	})
	if err != nil {
		t.Fatalf("voyage: %v", err)
	}
	// Боевые RAG-параметры (config.yaml: порог 0.40, top_k 5).
	retriever, err := rag.NewRetriever(config.RAGConfig{
		CosineThreshold: 0.40, TopK: 5, FallbackOnMiss: true,
	}, embedder, knowledge, ragAudit, log)
	if err != nil {
		t.Fatalf("retriever: %v", err)
	}

	// --- Очередь + воркер: боевая сборка, включая обработчик EP-03 ---
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
			Retriever:     retriever, // ← контур ТЗ §9 п.4: ответ из базы знаний
			Summaries:     summaries,
			SummaryEnq:    q,
			SummaryEveryN: 15,
			Prompt:        worker.NewPromptProvider(repo.NewEmmaPrompts(gormDB), log),
			Log:           log,
		}),
		worker.NewSummarizer(leads, msgs, summaries, ai, worker.NewRedisLocker(rdb), log),
		sender, 0, log)
	wrk.RegisterEmmaKB(worker.NewEmmaKBHandlers(worker.EmmaKBDeps{
		Files:   emmaKB,
		Indexer: rag.NewIndexer(embedder, knowledge, log),
		Pub:     pub,
		Log:     log,
	}))
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
	kbDir := t.TempDir()
	emma.NewKB(emma.KBDeps{Files: emmaKB, Enq: q, KBDir: kbDir, Log: log}).
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

	// --- Шаг 2: загрузка TXT с уникальным фактом через API панели ---
	fact := "Секретный код партнёрской программы INTERFIN GROUP: " + kbFactCode +
		". Этот код сотрудник называет клиенту только по прямому вопросу о коде партнёрской программы."
	var mpBody bytes.Buffer
	mw := multipart.NewWriter(&mpBody)
	fw, err := mw.CreateFormFile("file", kbFilename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(fact)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/emma/kb", &mpBody)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := do(req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var uploaded struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	if uploaded.Status != models.EmmaKBPending {
		t.Fatalf("после upload: %+v", uploaded)
	}

	// --- Шаг 3: ждём indexed (живой Voyage; free tier может отдать 429 —
	// воркер ретраится сам) ---
	waitStatus := func(want string, within time.Duration) *models.EmmaKBFile {
		t.Helper()
		deadline := time.Now().Add(within)
		for time.Now().Before(deadline) {
			f, err := emmaKB.GetByID(ctx, uploaded.ID)
			if err != nil {
				t.Fatalf("файл пропал: %v", err)
			}
			if f.IndexStatus == models.EmmaKBError {
				t.Fatalf("индексация упала: %v", *f.IndexError)
			}
			if f.IndexStatus == want {
				return f
			}
			time.Sleep(300 * time.Millisecond)
		}
		t.Fatalf("не дождались статуса %s", want)
		return nil
	}
	indexed := waitStatus(models.EmmaKBIndexed, 2*time.Minute)
	if indexed.ChunksCount < 1 {
		t.Fatalf("chunks_count = %d", indexed.ChunksCount)
	}
	t.Logf("файл проиндексирован: %d чанков", indexed.ChunksCount)

	// --- Шаг 4: лид спрашивает Эмму про факт из файла ---
	update := fmt.Sprintf(`{
		"update_id": 2,
		"message": {
			"message_id": 778,
			"date": %d,
			"text": "Подскажите, какой секретный код партнёрской программы INTERFIN GROUP?",
			"from": {"id": %d, "is_bot": false, "first_name": "Смоук", "username": "e2e_kb_lead"},
			"chat": {"id": %d, "type": "private"}
		}
	}`, time.Now().Unix(), kbLeadTgID, kbLeadTgID)
	req = httptest.NewRequest(http.MethodPost, "/webhook/telegram", bytes.NewBufferString(update))
	req.Header.Set(handlers.SecretTokenHeader, webhookSecret)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook: %d %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(90 * time.Second)
	for mock.sentCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if mock.sentCount() == 0 {
		t.Fatal("ответ лиду не отправлен за 90с")
	}
	reply := mock.lastSent()
	t.Logf("ответ Эммы: %q", reply.Text)
	// Факт существует ТОЛЬКО в загруженном файле: код в ответе доказывает
	// цепочку панель → диск → asynq → Voyage → pgvector → RAG → Claude.
	if !strings.Contains(reply.Text, kbFactCode) {
		t.Fatalf("ответ не содержит факт %s из загруженного файла: %q", kbFactCode, reply.Text)
	}

	// --- Шаг 5: удаление через API — чанков source не остаётся ---
	req = httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/api/emma/kb/%d", uploaded.ID), nil)
	if w := do(req); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	var left int64
	if err := gormDB.Model(&models.KnowledgeChunk{}).
		Where("source = ?", models.EmmaKBSource(uploaded.ID)).
		Count(&left).Error; err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("после DELETE осталось %d чанков", left)
	}
}
