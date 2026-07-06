package repo

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/models"
)

// Интеграционные тесты против реального PostgreSQL со схемой из migrations/.
// Локально: docker compose up -d postgres; go run ./cmd/migrate up;
// POSTGRES_TEST_DSN=postgres://postgres:postgres@localhost:5432/interfin?sslmode=disable go test ./internal/repo
// Без POSTGRES_TEST_DSN — skip (юнит-прогон не требует БД).

func testRepos(t *testing.T) (LeadRepo, MessageRepo, PaymentRepo) {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN не задан — интеграционный тест пропущен")
	}
	gdb, err := db.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	// Каждый прогон — с чистого листа (TRUNCATE вместо down/up: быстрее).
	if err := gdb.Exec(`TRUNCATE leads, messages, payment_events RESTART IDENTITY CASCADE`).Error; err != nil {
		t.Fatalf("truncate: %v (схема накатана? go run ./cmd/migrate up)", err)
	}
	l, m, p := New(gdb)
	return l, m, p
}

func TestInboundOnlyCounters(t *testing.T) {
	leads, msgs, _ := testRepos(t)
	ctx := context.Background()

	lead := &models.Lead{TelegramUserID: 111}
	if err := leads.Create(ctx, lead); err != nil {
		t.Fatal(err)
	}

	// 2 inbound + 3 outbound.
	for i := 0; i < 2; i++ {
		if err := msgs.CreateInbound(ctx, &models.Message{LeadID: lead.ID, Content: "от лида"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := msgs.CreateOutbound(ctx, &models.Message{LeadID: lead.ID, Content: "ответ бота"}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := leads.GetByID(ctx, lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	// CLAUDE.md §4.3: считаются ТОЛЬКО inbound.
	if got.MessageCount != 2 {
		t.Errorf("message_count = %d, ждали 2 (только inbound)", got.MessageCount)
	}
	if got.AntiSpamCount != 2 {
		t.Errorf("anti_spam_count = %d, ждали 2", got.AntiSpamCount)
	}

	history, err := msgs.ListByLead(ctx, lead.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Fatalf("ListByLead: %d сообщений, ждали 3", len(history))
	}
	if history[0].ID >= history[len(history)-1].ID {
		t.Error("ListByLead: порядок должен быть старые → новые")
	}
}

func TestDirectionCheckConstraint(t *testing.T) {
	leads, _, _ := testRepos(t)
	ctx := context.Background()

	lead := &models.Lead{TelegramUserID: 222}
	if err := leads.Create(ctx, lead); err != nil {
		t.Fatal(err)
	}
	// CHECK из §8.2 держит удар: прямая вставка мимо CreateInbound/Outbound.
	gdb, err := db.Open(config.DatabaseConfig{DSN: os.Getenv("POSTGRES_TEST_DSN"), MaxOpenConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	insert := gdb.Exec(`INSERT INTO messages (lead_id, direction, content) VALUES (?, 'sideways', 'x')`, lead.ID)
	if insert.Error == nil {
		t.Fatal("CHECK (direction IN ('inbound','outbound')) не сработал")
	}
}

func TestLeadNotFoundAndUpdateFields(t *testing.T) {
	leads, _, _ := testRepos(t)
	ctx := context.Background()

	if _, err := leads.GetByTelegramUserID(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("ждали ErrNotFound, получили %v", err)
	}

	lead := &models.Lead{TelegramUserID: 333}
	if err := leads.Create(ctx, lead); err != nil {
		t.Fatal(err)
	}
	// §11.2: Redis down → pending_task=TRUE.
	if err := leads.UpdateFields(ctx, lead.ID, map[string]interface{}{"pending_task": true}); err != nil {
		t.Fatal(err)
	}
	got, err := leads.GetByID(ctx, lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.PendingTask {
		t.Error("pending_task не выставился")
	}
}

func TestPaymentEvents(t *testing.T) {
	leads, _, pays := testRepos(t)
	ctx := context.Background()

	lead := &models.Lead{TelegramUserID: 444}
	if err := leads.Create(ctx, lead); err != nil {
		t.Fatal(err)
	}
	ok := true
	gw, st, cur := "nowpayments", "finished", "USDT"
	ev := &models.PaymentEvent{
		LeadID: lead.ID, Gateway: &gw, Status: &st, Currency: &cur,
		ToleranceOk: &ok, RawPayload: models.JSONB(`{"k":"v"}`),
	}
	if err := pays.Create(ctx, ev); err != nil {
		t.Fatal(err)
	}
	evs, err := pays.ListByLead(ctx, lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].ToleranceOk == nil || !*evs[0].ToleranceOk {
		t.Fatalf("payment_events: %+v", evs)
	}
}

// TestPaymentEventsDedup — M6: ретрай платёжного вебхука с тем же update_id
// не плодит вторую фискальную запись (уникальный индекс 0008 +
// ON CONFLICT DO NOTHING в Create), а платёж другого шлюза с совпадающим
// update_id — не задевает.
func TestPaymentEventsDedup(t *testing.T) {
	leads, _, pays := testRepos(t)
	ctx := context.Background()

	lead := &models.Lead{TelegramUserID: 666}
	if err := leads.Create(ctx, lead); err != nil {
		t.Fatal(err)
	}

	gw, cur := "cryptobot", "USDT"
	mk := func(gateway string) *models.PaymentEvent {
		return &models.PaymentEvent{
			LeadID: lead.ID, Gateway: &gateway, Currency: &cur,
			RawPayload: models.JSONB(`{"update_id": 42, "update_type": "invoice_paid"}`),
		}
	}
	if err := pays.Create(ctx, mk(gw)); err != nil {
		t.Fatal(err)
	}
	// Повтор того же update_id того же шлюза — no-op без ошибки.
	if err := pays.Create(ctx, mk(gw)); err != nil {
		t.Fatalf("повторная вставка должна быть no-op, получили: %v", err)
	}
	// Тот же update_id ДРУГОГО шлюза — самостоятельная запись.
	if err := pays.Create(ctx, mk("otherpay")); err != nil {
		t.Fatal(err)
	}

	evs, err := pays.ListByLead(ctx, lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("ожидали 2 записи (дубль отсеян, чужой шлюз прошёл), получили %d", len(evs))
	}
}

// TestTransitionStage — CAS-переход M5 против реального Postgres:
// guard по from (приоритет §3.1), атомарный сброс anti_spam_count (§3.5)
// и обновление last_activity_at — точки отсчёта TTL (CLAUDE.md §4.7).
func TestTransitionStage(t *testing.T) {
	leads, msgs, _ := testRepos(t)
	ctx := context.Background()

	// StageID задан явно, как в боевом создании лида (handlers/telegram.go):
	// GORM вставляет zero value поверх DEFAULT 1 из схемы.
	lead := &models.Lead{TelegramUserID: 555, StageID: 1}
	if err := leads.Create(ctx, lead); err != nil {
		t.Fatal(err)
	}
	// Наращиваем счётчики и «старим» активность.
	for i := 0; i < 3; i++ {
		if err := msgs.CreateInbound(ctx, &models.Message{LeadID: lead.ID, Content: "спам"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := leads.UpdateFields(ctx, lead.ID,
		map[string]interface{}{"last_activity_at": time.Now().Add(-72 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	// CAS с верным from: переход применён, счётчик сброшен, активность свежая.
	ok, err := leads.TransitionStage(ctx, lead.ID, 1, 6)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("CAS с верным from обязан пройти")
	}
	got, err := leads.GetByID(ctx, lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StageID != 6 {
		t.Errorf("stage_id=%d, ожидали 6", got.StageID)
	}
	if got.AntiSpamCount != 0 {
		t.Errorf("anti_spam_count=%d, ожидали 0 (§3.5: сброс при переходе)", got.AntiSpamCount)
	}
	if got.MessageCount != 3 {
		t.Errorf("message_count=%d, ожидали 3 — переход стадии его НЕ трогает (§3.2)", got.MessageCount)
	}
	if time.Since(got.LastActivityAt) > time.Minute {
		t.Errorf("last_activity_at=%v не обновлён — TTL считался бы не от перехода (CLAUDE.md §4.7)",
			got.LastActivityAt)
	}

	// CAS с устаревшим from: false, состояние не тронуто (приоритет §3.1).
	ok, err = leads.TransitionStage(ctx, lead.ID, 1, 8)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("CAS с устаревшим from обязан промахнуться")
	}
	got, _ = leads.GetByID(ctx, lead.ID)
	if got.StageID != 6 {
		t.Errorf("проигравший CAS перетёр стадию: %d", got.StageID)
	}
}
