package handlers

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/kanban"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/payment"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// Сквозной интеграционный тест M6: подписанный HTTP-запрос проходит весь
// боевой стек — реальные Postgres (репозитории, миграции 0001–0008), Redis
// (nonce store §5.5), машина стадий M5 с реальным TTLManager (Asynq).
// Фейки только на границах наружу: Telegram-sender и pub/sub-паблишер.
//
// Требует POSTGRES_TEST_DSN и REDIS_TEST_ADDR (в CI заданы), иначе skip.

type nullSender struct{}

func (nullSender) Send(int64, string) error { return nil }

func testDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type payIntEnv struct {
	router *gin.Engine
	leads  repo.LeadRepo
	pays   repo.PaymentRepo
	pub    *fakePub
	ttlMgr *queue.TTLManager
	insp   *asynq.Inspector
}

func newPayIntEnv(t *testing.T) *payIntEnv {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	redisAddr := os.Getenv("REDIS_TEST_ADDR")
	if dsn == "" || redisAddr == "" {
		t.Skip("POSTGRES_TEST_DSN/REDIS_TEST_ADDR не заданы — интеграционный тест пропущен")
	}

	gdb, err := db.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec(`TRUNCATE leads, messages, payment_events RESTART IDENTITY CASCADE`).Error; err != nil {
		t.Fatalf("truncate: %v (схема накатана? go run ./cmd/migrate up)", err)
	}
	leads, msgs, pays := repo.New(gdb)

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	t.Cleanup(func() { rdb.Close() })
	// Nonce прошлых прогонов живут 10 мин — повторный запуск теста не должен
	// ловить 403 replay на собственных вчерашних ключах.
	ctx := context.Background()
	if keys, _ := rdb.Keys(ctx, "payment:nonce:*").Result(); len(keys) > 0 {
		rdb.Del(ctx, keys...)
	}

	redisCfg := config.RedisConfig{Addr: redisAddr}
	ttlMgr := queue.NewTTLManager(redisCfg, leads)
	spamMgr := queue.NewAntiSpamManager(redisCfg)
	t.Cleanup(func() {
		ttlMgr.Close()
		spamMgr.Close()
	})

	pub := &fakePub{}
	machine := kanban.NewMachine(leads, msgs, ttlMgr, spamMgr, pub, nullSender{},
		config.KanbanConfig{
			AntiSpamLimit:         25,
			AntiSpamFollowupHours: 24,
			AntiSpamEscalateHours: 48,
			TTLStage4Hours:        48,
			TTLStage6Days:         5,
		}, testDiscardLogger())

	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewPaymentWebhook(PaymentWebhookDeps{
		Leads:    leads,
		Payments: pays,
		Machine:  machine,
		Nonces:   payment.NewRedisNonceStore(rdb),
		Pub:      pub,
		Cfg: config.PaymentConfig{
			Gateway:             "cryptobot",
			TestnetToken:        payTestToken,
			UseTestnet:          true,
			ReplayWindowMinutes: 5,
		},
		TolerancePct: 2.0,
		Log:          testDiscardLogger(),
	}).Register(router)

	insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: redisAddr})
	t.Cleanup(func() { insp.Close() })

	return &payIntEnv{router: router, leads: leads, pays: pays, pub: pub, ttlMgr: ttlMgr, insp: insp}
}

func (e *payIntEnv) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/payment", bytes.NewBufferString(body))
	req.Header.Set(payment.SignatureHeader, payment.Sign(payTestToken, []byte(body)))
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// TestPaymentIntegration_FullFlow — критерии приёмки M6 одним боевым потоком.
func TestPaymentIntegration_FullFlow(t *testing.T) {
	e := newPayIntEnv(t)
	ctx := context.Background()

	// Два лида в Stage 2 («живой»), как перед оплатой в реальном потоке.
	paidLead := &models.Lead{TelegramUserID: 91001, StageID: kanban.StageLive}
	underLead := &models.Lead{TelegramUserID: 91002, StageID: kanban.StageLive}
	for _, l := range []*models.Lead{paidLead, underLead} {
		if err := e.leads.Create(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = e.ttlMgr.Cancel(ctx, underLead.ID) }) // не оставляем 48ч-задачу в dev-Redis

	// --- IQ-3, часть 1: USDT 99 из 100 → Stage 3 ---
	paidBody := invoicePaidJSON(9001, paidLead.ID, "100", "99.5", "0.5") // net = 99
	if w := e.post(t, paidBody); w.Code != http.StatusOK {
		t.Fatalf("оплата 99/100: ожидали 200, получили %d (%s)", w.Code, w.Body.String())
	}
	got, err := e.leads.GetByID(ctx, paidLead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StageID != kanban.StagePaid {
		t.Errorf("лид в БД в стадии %d, ожидали 3 (Оплачено)", got.StageID)
	}
	if got.ManualResolution {
		t.Error("оплата в tolerance не должна включать manual_resolution")
	}

	// payment_events в БД: суммы, net за вычетом fee, tolerance_ok, raw_payload.
	evs, err := e.pays.ListByLead(ctx, paidLead.ID)
	if err != nil || len(evs) != 1 {
		t.Fatalf("payment_events: %v, записей %d (ожидали 1)", err, len(evs))
	}
	ev := evs[0]
	if !ev.AmountDue.Decimal.Equal(decimal.NewFromInt(100)) ||
		!ev.AmountReceived.Decimal.Equal(decimal.RequireFromString("99.5")) ||
		!ev.NetReceived.Decimal.Equal(decimal.NewFromInt(99)) {
		t.Errorf("суммы в БД: due=%s received=%s net=%s, ожидали 100/99.5/99",
			ev.AmountDue.Decimal, ev.AmountReceived.Decimal, ev.NetReceived.Decimal)
	}
	if ev.ToleranceOk == nil || !*ev.ToleranceOk || ev.Gateway == nil || *ev.Gateway != "cryptobot" {
		t.Errorf("tolerance_ok/gateway в БД: %+v", ev)
	}
	if len(ev.RawPayload) == 0 {
		t.Error("raw_payload пуст — аудит §8.3 потерян")
	}

	// --- §5.5: повтор того же вебхука → 403, второй записи нет ---
	if w := e.post(t, paidBody); w.Code != http.StatusForbidden {
		t.Errorf("replay: ожидали 403, получили %d", w.Code)
	}
	if evs, _ := e.pays.ListByLead(ctx, paidLead.ID); len(evs) != 1 {
		t.Errorf("replay создал лишние записи: %d", len(evs))
	}

	// --- IQ-3, часть 2: USDT 97 из 100 → Stage 4 + manual_resolution + TTL 48ч ---
	underBody := invoicePaidJSON(9002, underLead.ID, "100", "97.5", "0.5") // net = 97
	if w := e.post(t, underBody); w.Code != http.StatusOK {
		t.Fatalf("недоплата 97/100: ожидали 200, получили %d (%s)", w.Code, w.Body.String())
	}
	got, err = e.leads.GetByID(ctx, underLead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StageID != kanban.StageUnpaid {
		t.Errorf("недоплативший лид в стадии %d, ожидали 4 (Не оплачено)", got.StageID)
	}
	if !got.ManualResolution {
		t.Error("manual_resolution=false в БД, ожидали true (§3.3)")
	}
	// TTL стадии 4 реально взведён в Asynq на ~48ч (§3.4).
	info, err := e.insp.GetTaskInfo("default", queue.TTLDedupKey(underLead.ID))
	if err != nil {
		t.Fatalf("ttl:expire не взведён для стадии 4: %v", err)
	}
	if d := time.Until(info.NextProcessAt); d < 48*time.Hour-time.Minute || d > 48*time.Hour+time.Minute {
		t.Errorf("TTL стадии 4 = %v, ожидали ~48ч", d)
	}

	// --- задача 6: события payment_received опубликованы для обоих платежей ---
	var received int
	for _, pev := range e.pub.events {
		if pev.Type == "payment_received" {
			received++
		}
	}
	if received != 2 {
		t.Errorf("payment_received опубликовано %d раз, ожидали 2", received)
	}
}
