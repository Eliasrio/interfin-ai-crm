// Интеграционные тесты EmmaKBRepo (EP-03) против реального PostgreSQL
// (POSTGRES_TEST_DSN, иначе skip):
//   - upsert по filename: id стабилен, вытесненный file_path возвращается;
//   - SetStatus: indexed/error, ErrNotFound на удалённой строке;
//   - Delete: транзакция «чанки source + строка» — критерий приёмки
//     «строка удалена, чанков source нет».
package repo

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

// testKBDB — testEmmaDB + зачистка knowledge_chunks (Delete трогает обе).
func testKBDB(t *testing.T) (*gorm.DB, EmmaKBRepo, KnowledgeRepo) {
	t.Helper()
	gdb := testEmmaDB(t)
	if err := gdb.Exec(`TRUNCATE knowledge_chunks RESTART IDENTITY`).Error; err != nil {
		t.Fatalf("truncate knowledge_chunks: %v", err)
	}
	knowledge, _, _ := NewRAG(gdb)
	return gdb, NewEmmaKB(gdb), knowledge
}

// kbVector — валидный вектор для vector(1024) (сами эмбеддинги — контур M4).
func kbVector() models.Vector {
	v := make(models.Vector, 1024)
	v[0] = 1
	return v
}

func TestEmmaKBUpsertByFilename(t *testing.T) {
	_, files, _ := testKBDB(t)
	ctx := context.Background()

	// Первая загрузка — вставка, вытеснять нечего.
	f1 := &models.EmmaKBFile{
		Filename: "prices.txt", MimeType: "text/plain",
		FilePath: "data/emma/kb/uuid-1", FileSize: 10,
	}
	oldPath, err := files.UpsertByFilename(ctx, f1)
	if err != nil {
		t.Fatal(err)
	}
	if oldPath != "" || f1.ID == 0 || f1.CreatedAt.IsZero() {
		t.Fatalf("вставка: oldPath=%q id=%d created_at=%v", oldPath, f1.ID, f1.CreatedAt)
	}

	// Файл проиндексировался и позже перезаливается.
	if err := files.SetStatus(ctx, f1.ID, models.EmmaKBIndexed, 5, nil); err != nil {
		t.Fatal(err)
	}
	f2 := &models.EmmaKBFile{
		Filename: "prices.txt", MimeType: "text/plain",
		FilePath: "data/emma/kb/uuid-2", FileSize: 20,
	}
	oldPath, err = files.UpsertByFilename(ctx, f2)
	if err != nil {
		t.Fatal(err)
	}
	// QA-фикс ТЗ §3: id тот же (source panel:<id> совпадает), старый путь
	// возвращён для удаления с диска, строка сброшена в pending.
	if f2.ID != f1.ID {
		t.Fatalf("id сменился: %d → %d", f1.ID, f2.ID)
	}
	if oldPath != "data/emma/kb/uuid-1" {
		t.Fatalf("oldPath = %q", oldPath)
	}
	got, err := files.GetByID(ctx, f2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.IndexStatus != models.EmmaKBPending || got.ChunksCount != 0 ||
		got.IndexError != nil || got.FilePath != "data/emma/kb/uuid-2" ||
		got.FileSize != 20 {
		t.Fatalf("строка после переупserta: %+v", got)
	}

	// Другое имя — отдельная строка.
	f3 := &models.EmmaKBFile{
		Filename: "faq.md", MimeType: "text/markdown",
		FilePath: "data/emma/kb/uuid-3", FileSize: 30,
	}
	if _, err := files.UpsertByFilename(ctx, f3); err != nil {
		t.Fatal(err)
	}
	if f3.ID == f1.ID {
		t.Fatal("разные filename слились в одну строку")
	}

	// List: новые → старые.
	list, err := files.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Filename != "faq.md" {
		t.Fatalf("list: %+v", list)
	}
}

func TestEmmaKBSetStatus(t *testing.T) {
	_, files, _ := testKBDB(t)
	ctx := context.Background()

	f := &models.EmmaKBFile{
		Filename: "a.txt", MimeType: "text/plain", FilePath: "p", FileSize: 1,
	}
	if _, err := files.UpsertByFilename(ctx, f); err != nil {
		t.Fatal(err)
	}

	errText := "PDF без текстового слоя, OCR не поддерживается"
	if err := files.SetStatus(ctx, f.ID, models.EmmaKBError, 0, &errText); err != nil {
		t.Fatal(err)
	}
	got, err := files.GetByID(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.IndexStatus != models.EmmaKBError || got.IndexError == nil || *got.IndexError != errText {
		t.Fatalf("после error: %+v", got)
	}

	if err := files.SetStatus(ctx, f.ID, models.EmmaKBIndexed, 7, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = files.GetByID(ctx, f.ID)
	if got.IndexStatus != models.EmmaKBIndexed || got.ChunksCount != 7 || got.IndexError != nil {
		t.Fatalf("после indexed: %+v", got)
	}

	if err := files.SetStatus(ctx, 9999, models.EmmaKBIndexed, 1, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("несуществующий id: %v, ждали ErrNotFound", err)
	}
}

// Критерий приёмки: DELETE — строка удалена, чанков source нет; чужие
// source не задеты.
func TestEmmaKBDeleteRemovesChunks(t *testing.T) {
	gdb, files, knowledge := testKBDB(t)
	ctx := context.Background()

	f := &models.EmmaKBFile{
		Filename: "a.txt", MimeType: "text/plain", FilePath: "p", FileSize: 1,
	}
	if _, err := files.UpsertByFilename(ctx, f); err != nil {
		t.Fatal(err)
	}
	source := models.EmmaKBSource(f.ID)

	// Чанки файла панели + чужой source (docs/kb) рядом.
	if err := knowledge.ReplaceSource(ctx, source, []models.KnowledgeChunk{
		{Content: "чанк 1", Embedding: kbVector()},
		{Content: "чанк 2", Embedding: kbVector()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := knowledge.ReplaceSource(ctx, "faq.md", []models.KnowledgeChunk{
		{Content: "чужой чанк", Embedding: kbVector()},
	}); err != nil {
		t.Fatal(err)
	}

	deleted, err := files.Delete(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.FilePath != "p" {
		t.Fatalf("deleted: %+v", deleted)
	}

	if _, err := files.GetByID(ctx, f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("строка жива: %v", err)
	}
	var n int64
	if err := gdb.Model(&models.KnowledgeChunk{}).
		Where("source = ?", source).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("осталось %d чанков source %s", n, source)
	}
	if err := gdb.Model(&models.KnowledgeChunk{}).
		Where("source = ?", "faq.md").Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("чужой source задет: %d чанков", n)
	}

	if _, err := files.Delete(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("несуществующий id: %v, ждали ErrNotFound", err)
	}
}
