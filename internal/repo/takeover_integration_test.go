// Интеграционные тесты M13 против реального PostgreSQL (0013–0014):
// поля режима диалога лида, settings upsert, проверка ответа менеджера.
// Гейт тот же, что у repo_integration_test.go: без POSTGRES_TEST_DSN — skip.
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

func testTakeoverRepos(t *testing.T) (LeadRepo, MessageRepo, SettingsRepo) {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN не задан — интеграционный тест пропущен")
	}
	gdb, err := db.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec(`TRUNCATE leads, messages, settings RESTART IDENTITY CASCADE`).Error; err != nil {
		t.Fatalf("truncate: %v (схема накатана? go run ./cmd/migrate up — нужны 0013–0014)", err)
	}
	l, m, _ := New(gdb)
	return l, m, NewSettings(gdb)
}

// TestLeadDialogModeRoundtrip — 0013: дефолт 'bot', UpdateFields ходит в обе
// стороны, NULL-поля снимаются.
func TestLeadDialogModeRoundtrip(t *testing.T) {
	leads, _, _ := testTakeoverRepos(t)
	ctx := context.Background()

	lead := &models.Lead{TelegramUserID: 131}
	if err := leads.Create(ctx, lead); err != nil {
		t.Fatal(err)
	}
	got, err := leads.GetByID(ctx, lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DialogMode != models.DialogModeBot || got.BotSilencedUntil != nil || got.TakenBy != nil {
		t.Fatalf("дефолты 0013: %+v", got)
	}

	// Менеджер взял диалог.
	until := time.Now().UTC().Add(30 * time.Minute)
	if err := leads.UpdateFields(ctx, lead.ID, map[string]interface{}{
		"dialog_mode": models.DialogModeHuman, "taken_by": int64(5),
		"bot_silenced_until": until,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = leads.GetByID(ctx, lead.ID)
	if got.DialogMode != models.DialogModeHuman || got.TakenBy == nil || *got.TakenBy != 5 ||
		got.BotSilencedUntil == nil {
		t.Fatalf("после take: %+v", got)
	}
	if !got.BotSilenced(time.Now()) {
		t.Error("BotSilenced обязан быть true в режиме human")
	}

	// Возврат Эмме: NULL-поля снимаются.
	if err := leads.UpdateFields(ctx, lead.ID, map[string]interface{}{
		"dialog_mode": models.DialogModeBot, "taken_by": nil, "bot_silenced_until": nil,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = leads.GetByID(ctx, lead.ID)
	if got.DialogMode != models.DialogModeBot || got.TakenBy != nil || got.BotSilencedUntil != nil {
		t.Fatalf("после возврата: %+v", got)
	}
	if got.BotSilenced(time.Now()) {
		t.Error("BotSilenced обязан быть false в режиме bot без паузы")
	}
}

// TestHasManagerOutboundAfter — проверка «менеджер ответил после inbound»
// (no-op-условие reminder/pickup): считаются только outbound c author
// manager:%, ответы бота и старые реплики — нет.
func TestHasManagerOutboundAfter(t *testing.T) {
	leads, msgs, _ := testTakeoverRepos(t)
	ctx := context.Background()

	lead := &models.Lead{TelegramUserID: 132}
	if err := leads.Create(ctx, lead); err != nil {
		t.Fatal(err)
	}
	manager := models.AuthorManagerPrefix + "3"
	bot := models.AuthorBot

	// Реплика менеджера ДО inbound не считается ответом.
	if err := msgs.CreateOutbound(ctx, &models.Message{LeadID: lead.ID, Author: &manager, Content: "старая"}); err != nil {
		t.Fatal(err)
	}
	inbound := &models.Message{LeadID: lead.ID, Content: "жду ответа"}
	if err := msgs.CreateInbound(ctx, inbound); err != nil {
		t.Fatal(err)
	}
	if replied, _ := msgs.HasManagerOutboundAfter(ctx, lead.ID, inbound.ID); replied {
		t.Fatal("реплика менеджера ДО inbound не должна считаться ответом")
	}

	// Ответ бота — не ответ менеджера.
	if err := msgs.CreateOutbound(ctx, &models.Message{LeadID: lead.ID, Author: &bot, Content: "бот"}); err != nil {
		t.Fatal(err)
	}
	if replied, _ := msgs.HasManagerOutboundAfter(ctx, lead.ID, inbound.ID); replied {
		t.Fatal("ответ бота не должен гасить напоминание")
	}

	// Ответ менеджера ПОСЛЕ inbound — считается.
	if err := msgs.CreateOutbound(ctx, &models.Message{LeadID: lead.ID, Author: &manager, Content: "отвечаю"}); err != nil {
		t.Fatal(err)
	}
	replied, err := msgs.HasManagerOutboundAfter(ctx, lead.ID, inbound.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !replied {
		t.Fatal("ответ менеджера после inbound обязан быть виден")
	}
}

// TestSettingsUpsert — 0014: Get без строки → ErrNotFound, Set вставляет
// и перезаписывает (upsert по PK key).
func TestSettingsUpsert(t *testing.T) {
	_, _, settings := testTakeoverRepos(t)
	ctx := context.Background()

	if _, err := settings.Get(ctx, "takeover.reminder_minutes"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("пустая таблица: ожидали ErrNotFound, получили %v", err)
	}
	if err := settings.Set(ctx, "takeover.reminder_minutes", "5"); err != nil {
		t.Fatal(err)
	}
	if v, err := settings.Get(ctx, "takeover.reminder_minutes"); err != nil || v != "5" {
		t.Fatalf("после вставки: %q, %v", v, err)
	}
	// Перезапись того же ключа — upsert, не duplicate key.
	if err := settings.Set(ctx, "takeover.reminder_minutes", "15"); err != nil {
		t.Fatal(err)
	}
	if v, _ := settings.Get(ctx, "takeover.reminder_minutes"); v != "15" {
		t.Fatalf("после перезаписи: %q, ожидали 15", v)
	}
}
