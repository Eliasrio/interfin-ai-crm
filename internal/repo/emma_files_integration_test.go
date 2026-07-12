// Интеграционные тесты EmmaSendFilesRepo (EP-04) против реального PostgreSQL
// (POSTGRES_TEST_DSN, иначе skip):
//   - CRUD + ListActive (порядок и фильтр is_active);
//   - критерий приёмки: DELETE — строки нет, а старые emma_events с этим
//     send_file_id живы с NULL (ON DELETE SET NULL, миграция 0020).
package repo

import (
	"context"
	"errors"
	"testing"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

func seedSendFile(t *testing.T, r EmmaSendFilesRepo, name string, active bool) *models.EmmaSendFile {
	t.Helper()
	f := &models.EmmaSendFile{
		Name:        name,
		Description: "отправь, когда клиент спрашивает " + name,
		FilePath:    "data/emma/files/uuid-" + name,
		MimeType:    "application/pdf",
		FileSize:    1024,
		IsActive:    active,
	}
	if err := r.Create(context.Background(), f); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return f
}

func TestSendFilesCRUD(t *testing.T) {
	gdb := testEmmaDB(t)
	r := NewEmmaSendFiles(gdb)
	ctx := context.Background()

	active := seedSendFile(t, r, "прайс", true)
	inactive := seedSendFile(t, r, "фото", false)

	if active.ID == 0 || active.CreatedAt.IsZero() {
		t.Fatalf("Create не заполнил ID/CreatedAt: %+v", active)
	}
	// DEFAULT TRUE не перетёрт явным false (грабля M5).
	gotInactive, err := r.GetByID(ctx, inactive.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotInactive.IsActive {
		t.Error("is_active=false не сохранился (zero value vs DEFAULT TRUE)")
	}

	// List — все; ListActive — только активные, id ASC.
	all, err := r.List(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("List: %v, %v", all, err)
	}
	activeList, err := r.ListActive(ctx)
	if err != nil || len(activeList) != 1 || activeList[0].ID != active.ID {
		t.Fatalf("ListActive: %+v, %v", activeList, err)
	}

	// Update: частичный (только is_active), затем name+description.
	off := false
	upd, err := r.Update(ctx, active.ID, EmmaSendFileUpdate{IsActive: &off})
	if err != nil || upd.IsActive || upd.Name != "прайс" {
		t.Fatalf("Update is_active: %+v, %v", upd, err)
	}
	// Критерий приёмки: выключенный файл пропадает из ListActive
	// (валидатор маркеров и секция промпта его больше не видят).
	activeList, err = r.ListActive(ctx)
	if err != nil || len(activeList) != 0 {
		t.Fatalf("ListActive после выключения: %+v, %v", activeList, err)
	}
	name, desc := "Прайс 2027", "новые цены"
	upd, err = r.Update(ctx, active.ID, EmmaSendFileUpdate{Name: &name, Description: &desc})
	if err != nil || upd.Name != name || upd.Description != desc || upd.IsActive {
		t.Fatalf("Update name/description: %+v, %v", upd, err)
	}

	// Update/GetByID несуществующего → ErrNotFound.
	if _, err := r.Update(ctx, 9999, EmmaSendFileUpdate{Name: &name}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update 9999: %v", err)
	}
	if _, err := r.GetByID(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByID 9999: %v", err)
	}
}

// Критерий приёмки EP-04: после DELETE строки нет, а события журнала живы
// с send_file_id = NULL (статистика EP-06 переживает удаление файла).
func TestSendFileDeleteKeepsEventsWithNull(t *testing.T) {
	gdb := testEmmaDB(t)
	r := NewEmmaSendFiles(gdb)
	events := NewEmmaEvents(gdb)
	ctx := context.Background()

	f := seedSendFile(t, r, "прайс", true)

	ev := &models.EmmaEvent{EventType: models.EmmaEventFileSent, SendFileID: &f.ID}
	if err := events.Create(ctx, ev); err != nil {
		t.Fatalf("event: %v", err)
	}

	deleted, err := r.Delete(ctx, f.ID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	// RETURNING-контракт: file_path для удаления оригинала с диска.
	if deleted.FilePath != f.FilePath {
		t.Errorf("file_path = %q, ждали %q", deleted.FilePath, f.FilePath)
	}

	// Строки нет; повторный Delete → ErrNotFound.
	if _, err := r.GetByID(ctx, f.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("строка пережила Delete: %v", err)
	}
	if _, err := r.Delete(ctx, f.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("повторный Delete: %v", err)
	}

	// Событие живо, send_file_id обнулён БД (ON DELETE SET NULL).
	var got models.EmmaEvent
	if err := gdb.First(&got, ev.ID).Error; err != nil {
		t.Fatalf("событие не пережило удаление файла: %v", err)
	}
	if got.SendFileID != nil {
		t.Errorf("send_file_id = %v, ждали NULL", *got.SendFileID)
	}
	if got.EventType != models.EmmaEventFileSent {
		t.Errorf("event_type = %q", got.EventType)
	}
}
