// Интеграционные тесты M8 против реального PostgreSQL (схема migrations/):
//   - AQ²-4: erasure анонимизирует лида, хеширует telegram_user_id,
//     сохраняет payment_events;
//   - catch-up §10.3: LeadRepo.List с updated_since;
//   - retention §9.1: физическое удаление стёртых > cutoff, платежи выживают.
//
// Запуск как у repo_integration_test.go: POSTGRES_TEST_DSN, иначе skip.
package repo

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/lgpd"
	"github.com/interfin/interfin-ai-crm/internal/models"
)

// testDB — прямой *gorm.DB: тестам ретеншена нужно руками старить deleted_at.
func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN не задан — интеграционный тест пропущен")
	}
	gdb, err := db.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec(
		`TRUNCATE leads, messages, payment_events, lgpd_audit RESTART IDENTITY CASCADE`,
	).Error; err != nil {
		t.Fatalf("truncate: %v (схема накатана? go run ./cmd/migrate up)", err)
	}
	return gdb
}

func mkLead(t *testing.T, leads LeadRepo, tgID int64) *models.Lead {
	t.Helper()
	name, phone, username := "Иван", "+5521999999999", "ivan"
	lead := &models.Lead{
		TelegramUserID: tgID, Name: &name, Phone: &phone, TgUsername: &username,
		StageID: 1, // явный StageID: zero value затирал бы DEFAULT схемы (грабля M5)
	}
	if err := leads.Create(context.Background(), lead); err != nil {
		t.Fatal(err)
	}
	return lead
}

func TestListUpdatedSince(t *testing.T) {
	gdb := testDB(t)
	leads, _, _ := New(gdb)
	ctx := context.Background()

	old := mkLead(t, leads, 111)
	fresh := mkLead(t, leads, 222)
	freshest := mkLead(t, leads, 333)
	// Активность разносится руками: -2ч, -30м, сейчас.
	for id, ago := range map[int64]time.Duration{
		old.ID: 2 * time.Hour, fresh.ID: 30 * time.Minute, freshest.ID: 0,
	} {
		if err := leads.UpdateFields(ctx, id, map[string]interface{}{
			"last_activity_at": time.Now().UTC().Add(-ago),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Без фильтра: все трое, свежие первыми.
	all, total, err := leads.List(ctx, ListLeadsParams{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(all) != 3 {
		t.Fatalf("total=%d len=%d, ждали 3/3", total, len(all))
	}
	if all[0].ID != freshest.ID || all[2].ID != old.ID {
		t.Fatalf("порядок не last_activity_at DESC: %d,%d,%d", all[0].ID, all[1].ID, all[2].ID)
	}

	// Catch-up §10.3: изменённые за последний час — двое.
	since := time.Now().UTC().Add(-time.Hour)
	got, total, err := leads.List(ctx, ListLeadsParams{UpdatedSince: &since, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(got) != 2 {
		t.Fatalf("updated_since: total=%d len=%d, ждали 2/2", total, len(got))
	}
	for _, l := range got {
		if l.ID == old.ID {
			t.Fatal("updated_since вернул лида со старой активностью")
		}
	}

	// Пагинация: страница 1 c limit=1 — самый свежий; total не меняется.
	page, total, err := leads.List(ctx, ListLeadsParams{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(page) != 1 || page[0].ID != fresh.ID {
		t.Fatalf("пагинация: total=%d len=%d id=%v", total, len(page), page)
	}
}

// TestEraseAnonymizesAndPreservesFiscal — AQ²-4 (P0, integration).
func TestEraseAnonymizesAndPreservesFiscal(t *testing.T) {
	gdb := testDB(t)
	leads, msgs, payments := New(gdb)
	lgpdRepo := NewLGPD(gdb)
	ctx := context.Background()

	lead := mkLead(t, leads, 555000111)
	for _, text := range []string{"хочу консультацию", "мой телефон +55..."} {
		if err := msgs.CreateInbound(ctx, &models.Message{LeadID: lead.ID, Content: text}); err != nil {
			t.Fatal(err)
		}
	}
	gw, status := "cryptobot", "paid"
	raw := models.JSONB(`{"update_id": 42}`)
	if err := payments.Create(ctx, &models.PaymentEvent{
		LeadID: lead.ID, Gateway: &gw, Status: &status, RawPayload: raw,
	}); err != nil {
		t.Fatal(err)
	}

	hashed := lgpd.HashTelegramUserID(lead.TelegramUserID, "test-salt")
	if err := lgpdRepo.Erase(ctx, lead.ID, hashed, "manager:1", "10.0.0.1"); err != nil {
		t.Fatal(err)
	}

	// Лид: soft delete + NULL PII + хешированный telegram_user_id (не NULL).
	got, err := lgpdRepo.GetLeadAny(ctx, lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.DeletedAt.Valid {
		t.Fatal("deleted_at не выставлен")
	}
	if got.Name != nil || got.Phone != nil || got.TgUsername != nil {
		t.Fatalf("PII не обнулены: %+v", got)
	}
	if got.TelegramUserID != hashed || got.TelegramUserID >= 0 {
		t.Fatalf("telegram_user_id = %d, ждали хеш %d", got.TelegramUserID, hashed)
	}
	// Для бизнес-кода лид исчез (soft-delete-скоуп).
	if _, err := leads.GetByID(ctx, lead.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID стёртого лида: %v, ждали ErrNotFound", err)
	}

	// Сообщения затёрты.
	msgRows, err := msgs.ListByLead(ctx, lead.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgRows) != 2 {
		t.Fatalf("сообщений %d, ждали 2", len(msgRows))
	}
	for _, m := range msgRows {
		if m.Content != lgpd.DeletedContent {
			t.Fatalf("content = %q, ждали %q", m.Content, lgpd.DeletedContent)
		}
	}

	// payment_events НЕ тронуты (фискальная retention §9.3).
	evs, err := payments.ListByLead(ctx, lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Gateway == nil || *evs[0].Gateway != "cryptobot" {
		t.Fatalf("payment_events повреждены: %+v", evs)
	}

	// След в lgpd_audit (§9.2).
	audit, err := lgpdRepo.ListAuditByLead(ctx, lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 || audit[0].Action != lgpd.ActionErase || audit[0].PerformedBy != "manager:1" {
		t.Fatalf("lgpd_audit: %+v", audit)
	}

	// Повторный erase — ErrNotFound, вторая audit-запись не появляется.
	if err := lgpdRepo.Erase(ctx, lead.ID, hashed, "manager:1", "10.0.0.1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("повторный erase: %v, ждали ErrNotFound", err)
	}
	if audit, _ = lgpdRepo.ListAuditByLead(ctx, lead.ID); len(audit) != 1 {
		t.Fatalf("повторный erase оставил след: %d записей", len(audit))
	}
}

// TestRetentionDeleteErasedBefore — §9.1: cron удаляет стёртых > 90 дней,
// платежи выживают с lead_id=NULL (FK SET NULL, миграция 0010).
func TestRetentionDeleteErasedBefore(t *testing.T) {
	gdb := testDB(t)
	leads, msgs, payments := New(gdb)
	lgpdRepo := NewLGPD(gdb)
	ctx := context.Background()

	oldErased := mkLead(t, leads, 111)
	newErased := mkLead(t, leads, 222)
	alive := mkLead(t, leads, 333)

	if err := msgs.CreateInbound(ctx, &models.Message{LeadID: oldErased.ID, Content: "x"}); err != nil {
		t.Fatal(err)
	}
	gw := "cryptobot"
	raw := models.JSONB(`{"update_id": 7}`)
	if err := payments.Create(ctx, &models.PaymentEvent{LeadID: oldErased.ID, Gateway: &gw, RawPayload: raw}); err != nil {
		t.Fatal(err)
	}

	for _, id := range []int64{oldErased.ID, newErased.ID} {
		tg, _ := lgpdRepo.GetLeadAny(ctx, id)
		if err := lgpdRepo.Erase(ctx, id,
			lgpd.HashTelegramUserID(tg.TelegramUserID, "s"), "manager:1", ""); err != nil {
			t.Fatal(err)
		}
	}
	// Старим первого: deleted_at = 91 день назад.
	if err := gdb.Exec(
		`UPDATE leads SET deleted_at = NOW() - INTERVAL '91 days' WHERE id = ?`,
		oldErased.ID,
	).Error; err != nil {
		t.Fatal(err)
	}

	deleted, err := lgpdRepo.DeleteErasedBefore(ctx, time.Now().UTC().AddDate(0, 0, -90))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("удалено %d, ждали 1", deleted)
	}

	// Старый стёртый исчез физически, свежестёртый и живой — на месте.
	if _, err := lgpdRepo.GetLeadAny(ctx, oldErased.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("старый лид не удалён: %v", err)
	}
	if _, err := lgpdRepo.GetLeadAny(ctx, newErased.ID); err != nil {
		t.Fatalf("свежестёртый лид пропал: %v", err)
	}
	if _, err := leads.GetByID(ctx, alive.ID); err != nil {
		t.Fatalf("живой лид пропал: %v", err)
	}

	// Сообщения ушли каскадом, платёж выжил с lead_id=NULL (§9.3).
	if rows, _ := msgs.ListByLead(ctx, oldErased.ID, 0); len(rows) != 0 {
		t.Fatalf("messages не удалены каскадом: %d", len(rows))
	}
	var evCount int64
	if err := gdb.Model(&models.PaymentEvent{}).
		Where("lead_id IS NULL AND gateway = ?", "cryptobot").
		Count(&evCount).Error; err != nil {
		t.Fatal(err)
	}
	if evCount != 1 {
		t.Fatalf("payment_events после retention: %d с lead_id=NULL, ждали 1", evCount)
	}
}

// TestEraseRepeatedCycle — баг живого стенда M10: create→erase→create→erase
// одного telegram_user_id. Хеш детерминирован (§9.3), поэтому второй erase
// пишет тот же хеш, что уже лежит в первой стёртой строке; глобальный UNIQUE
// из 0003 падал с 23505 и DELETE /api/lgpd/leads/:id/erase отвечал 500.
// После 0011 уникальность — только по живым строкам: цикл проходит, обе
// стёртые строки сосуществуют с одним хешом, дедуп живых сохраняется.
func TestEraseRepeatedCycle(t *testing.T) {
	gdb := testDB(t)
	leads, _, _ := New(gdb)
	lgpdRepo := NewLGPD(gdb)
	ctx := context.Background()

	const tgID = int64(777000555)
	hashed := lgpd.HashTelegramUserID(tgID, "test-salt")

	first := mkLead(t, leads, tgID)
	if err := lgpdRepo.Erase(ctx, first.ID, hashed, "manager:1", ""); err != nil {
		t.Fatal(err)
	}

	// Человек вернулся: новый лид с тем же telegram_user_id создаётся
	// (стёртая строка держит хеш, не исходный ID — конфликта нет).
	second := mkLead(t, leads, tgID)
	if second.ID == first.ID {
		t.Fatal("ожидали новую строку, а не реюз старой")
	}

	// Повторный erase — тот же детерминированный хеш. До 0011: 23505.
	if err := lgpdRepo.Erase(ctx, second.ID, hashed, "manager:1", ""); err != nil {
		t.Fatalf("erase вернувшегося лида: %v", err)
	}

	// Обе стёртые строки сосуществуют с одинаковым хешом (до retention §9.1).
	for _, id := range []int64{first.ID, second.ID} {
		got, err := lgpdRepo.GetLeadAny(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !got.DeletedAt.Valid || got.TelegramUserID != hashed {
			t.Fatalf("лид %d: deleted_at.Valid=%v tg=%d, ждали хеш %d",
				id, got.DeletedAt.Valid, got.TelegramUserID, hashed)
		}
	}
	// Живых с этим telegram_user_id не осталось.
	if _, err := leads.GetByTelegramUserID(ctx, tgID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByTelegramUserID: %v, ждали ErrNotFound", err)
	}
	// По одной audit-записи на каждый erase (§9.2).
	for _, id := range []int64{first.ID, second.ID} {
		if audit, _ := lgpdRepo.ListAuditByLead(ctx, id); len(audit) != 1 {
			t.Fatalf("lgpd_audit лида %d: %d записей, ждали 1", id, len(audit))
		}
	}

	// Дедуп ЖИВЫХ лидов не потерян: третий цикл создаётся, дубль — нет.
	third := mkLead(t, leads, tgID)
	dup := &models.Lead{TelegramUserID: tgID, StageID: 1}
	if err := leads.Create(ctx, dup); err == nil {
		t.Fatal("второй активный лид с тем же telegram_user_id создался — уникальность живых потеряна")
	}
	if _, err := leads.GetByID(ctx, third.ID); err != nil {
		t.Fatal(err)
	}
}
