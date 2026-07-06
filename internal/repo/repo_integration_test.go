package repo

import (
	"context"
	"errors"
	"os"
	"testing"

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
