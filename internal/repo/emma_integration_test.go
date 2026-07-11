// Интеграционные тесты EP-01 против реального PostgreSQL (схема 0016–0020,
// POSTGRES_TEST_DSN, иначе skip):
//   - GORM-модели панели Эммы совместимы со схемой (дефолты BOOLEAN/JSONB —
//     класс граблей M13 dialog_mode);
//   - критерий приёмки: два лида в emma_events c ON DELETE SET NULL ведут
//     себя корректно при erasure (soft delete события не трогает, физическое
//     удаление retention-cron'ом обнуляет только lead_id удалённого).
package repo

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

// testEmmaDB — testDB + зачистка таблиц панели Эммы.
func testEmmaDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb := testDB(t)
	if err := gdb.Exec(
		`TRUNCATE emma_events, emma_prompt_versions, emma_kb_files,
		 emma_send_files, emma_contacts RESTART IDENTITY CASCADE`,
	).Error; err != nil {
		t.Fatalf("truncate emma_*: %v (миграции 0016–0020 накатаны?)", err)
	}
	return gdb
}

// TestEmmaModelsMatchSchema — вставка/чтение каждой модели EP-01: дефолты
// БД доезжают (style, forbidden_topics, index_status), теги не зеркалят
// схему криво.
func TestEmmaModelsMatchSchema(t *testing.T) {
	gdb := testEmmaDB(t)

	// Пустые ForbiddenTopics/Style → дефолты БД '[]' и 'neutral'.
	v := &models.EmmaPromptVersion{SystemPrompt: "Ты — Эмма.", IsCurrent: true}
	if err := gdb.Create(v).Error; err != nil {
		t.Fatalf("prompt version: %v", err)
	}
	var gotV models.EmmaPromptVersion
	if err := gdb.First(&gotV, v.ID).Error; err != nil {
		t.Fatal(err)
	}
	if gotV.Style != models.EmmaStyleNeutral {
		t.Errorf("style = %q, ждали дефолт neutral", gotV.Style)
	}
	if string(gotV.ForbiddenTopics) != "[]" {
		t.Errorf("forbidden_topics = %q, ждали дефолт []", gotV.ForbiddenTopics)
	}
	if !gotV.IsCurrent {
		t.Error("is_current не сохранился")
	}

	// Частичный уникальный индекс: вторая is_current=true — ошибка.
	if err := gdb.Create(&models.EmmaPromptVersion{
		SystemPrompt: "v2", IsCurrent: true,
	}).Error; err == nil {
		t.Error("две активные версии промпта прошли мимо emma_prompt_current_key")
	}
	// А неактивных — сколько угодно.
	if err := gdb.Create(&models.EmmaPromptVersion{SystemPrompt: "v3"}).Error; err != nil {
		t.Errorf("неактивная версия: %v", err)
	}

	kb := &models.EmmaKBFile{
		Filename: "prices.pdf", MimeType: "application/pdf",
		FilePath: "data/emma/kb/uuid-1", FileSize: 1024,
	}
	if err := gdb.Create(kb).Error; err != nil {
		t.Fatalf("kb file: %v", err)
	}
	var gotKB models.EmmaKBFile
	if err := gdb.First(&gotKB, kb.ID).Error; err != nil {
		t.Fatal(err)
	}
	if gotKB.IndexStatus != models.EmmaKBPending {
		t.Errorf("index_status = %q, ждали дефолт pending", gotKB.IndexStatus)
	}
	// UNIQUE(filename): повторная загрузка того же имени — конфликт.
	if err := gdb.Create(&models.EmmaKBFile{
		Filename: "prices.pdf", MimeType: "application/pdf",
		FilePath: "data/emma/kb/uuid-2", FileSize: 1,
	}).Error; err == nil {
		t.Error("дубль filename прошёл мимо UNIQUE")
	}

	sf := &models.EmmaSendFile{
		Name: "Прайс", Description: "отправь при вопросе о ценах",
		FilePath: "data/emma/files/uuid-3", MimeType: "application/pdf",
		FileSize: 2048, IsActive: true, // явно: zero value затёр бы DEFAULT TRUE (грабля M5)
	}
	if err := gdb.Create(sf).Error; err != nil {
		t.Fatalf("send file: %v", err)
	}

	contact := &models.EmmaContact{
		Type: "whatsapp", Name: "Менеджер", Value: "+5521999999999", IsActive: true,
	}
	if err := gdb.Create(contact).Error; err != nil {
		t.Fatalf("contact: %v", err)
	}
	// CHECK(type): мусорный тип не проходит.
	if err := gdb.Create(&models.EmmaContact{
		Type: "pigeon", Name: "x", Value: "y", IsActive: true,
	}).Error; err == nil {
		t.Error("type=pigeon прошёл мимо CHECK")
	}
}

// TestEmmaEventsSurviveErasure — критерий приёмки EP-01: события двух лидов
// при erasure (soft) не трогаются, при физическом удалении retention-cron'ом
// lead_id обнуляется ТОЛЬКО у стёртого лида; send_file_id — SET NULL при
// удалении файла.
func TestEmmaEventsSurviveErasure(t *testing.T) {
	gdb := testEmmaDB(t)
	leads, _, _ := New(gdb)
	lgpdRepo := NewLGPD(gdb)
	ctx := context.Background()

	erased := mkLead(t, leads, 555000201)
	alive := mkLead(t, leads, 555000202)

	sf := &models.EmmaSendFile{
		Name: "Прайс", Description: "цены", FilePath: "data/emma/files/u1",
		MimeType: "application/pdf", FileSize: 1, IsActive: true,
	}
	if err := gdb.Create(sf).Error; err != nil {
		t.Fatal(err)
	}

	mkEvent := func(leadID int64, withFile bool) int64 {
		ev := &models.EmmaEvent{EventType: models.EmmaEventReply, LeadID: &leadID}
		if withFile {
			ev.EventType = models.EmmaEventFileSent
			ev.SendFileID = &sf.ID
		}
		if err := gdb.Create(ev).Error; err != nil {
			t.Fatal(err)
		}
		return ev.ID
	}
	evErased := mkEvent(erased.ID, true)
	evAlive := mkEvent(alive.ID, false)

	// Erasure (soft delete + анонимизация): события НЕ трогаются — FK
	// срабатывает только на физическом DELETE.
	if err := lgpdRepo.Erase(ctx, erased.ID, -111222333, "test", ""); err != nil {
		t.Fatal(err)
	}
	// Свежая переменная на каждый First: GORM подмешивает заполненный
	// первичный ключ структуры в условия следующего запроса.
	fetch := func(id int64) models.EmmaEvent {
		var ev models.EmmaEvent
		if err := gdb.First(&ev, id).Error; err != nil {
			t.Fatalf("emma_event id=%d: %v", id, err)
		}
		return ev
	}

	ev := fetch(evErased)
	if ev.LeadID == nil || *ev.LeadID != erased.ID {
		t.Fatalf("erasure тронул emma_events.lead_id: %v", ev.LeadID)
	}

	// Retention: физическое удаление стёртого лида → SET NULL только у него.
	deleted, err := lgpdRepo.DeleteErasedBefore(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("retention удалил %d лидов, ждали 1", deleted)
	}
	ev = fetch(evErased)
	if ev.LeadID != nil {
		t.Errorf("lead_id стёртого = %v, ждали NULL (ON DELETE SET NULL)", *ev.LeadID)
	}
	if ev.SendFileID == nil || *ev.SendFileID != sf.ID {
		t.Errorf("send_file_id пострадал от удаления лида: %v", ev.SendFileID)
	}

	ev = fetch(evAlive)
	if ev.LeadID == nil || *ev.LeadID != alive.ID {
		t.Errorf("lead_id живого лида = %v, ждали %d", ev.LeadID, alive.ID)
	}

	// Удаление файла из библиотеки: событие остаётся, send_file_id → NULL.
	if err := gdb.Delete(&models.EmmaSendFile{}, sf.ID).Error; err != nil {
		t.Fatal(err)
	}
	ev = fetch(evErased)
	if ev.SendFileID != nil {
		t.Errorf("send_file_id = %v, ждали NULL после удаления файла", *ev.SendFileID)
	}
}
