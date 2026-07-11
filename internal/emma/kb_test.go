// Тесты роутов /api/emma/kb* (EP-03): лимит 50 МБ → 413, allowlist +
// сигнатуры → 400, upsert по filename (id стабилен, старый оригинал
// удаляется с диска), reindex/delete, постановка emma:kb:index.
// Гейты (manager → 403, без PIN → 401) — общие тесты emma_test.go,
// kb-роуты включены в emmaPaths.
package emma

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// --- фейки EP-03 ---

// memKBRepo — in-memory repo.EmmaKBRepo с семантикой боевого upsert'а.
type memKBRepo struct {
	mu     sync.Mutex
	nextID int64
	files  map[int64]*models.EmmaKBFile
}

func newMemKBRepo() *memKBRepo {
	return &memKBRepo{files: map[int64]*models.EmmaKBFile{}}
}

func (r *memKBRepo) List(context.Context) ([]models.EmmaKBFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]models.EmmaKBFile, 0, len(r.files))
	for _, f := range r.files {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (r *memKBRepo) GetByID(_ context.Context, id int64) (*models.EmmaKBFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.files[id]
	if !ok {
		return nil, repo.ErrNotFound
	}
	cp := *f
	return &cp, nil
}

func (r *memKBRepo) UpsertByFilename(_ context.Context, f *models.EmmaKBFile) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, old := range r.files {
		if old.Filename == f.Filename {
			oldPath := old.FilePath
			old.MimeType, old.FilePath, old.FileSize = f.MimeType, f.FilePath, f.FileSize
			old.IndexStatus, old.IndexError, old.ChunksCount = models.EmmaKBPending, nil, 0
			f.ID, f.CreatedAt = old.ID, old.CreatedAt
			f.IndexStatus, f.IndexError, f.ChunksCount = models.EmmaKBPending, nil, 0
			return oldPath, nil
		}
	}
	r.nextID++
	f.ID = r.nextID
	f.CreatedAt = time.Now().UTC()
	f.IndexStatus, f.IndexError, f.ChunksCount = models.EmmaKBPending, nil, 0
	cp := *f
	r.files[f.ID] = &cp
	return "", nil
}

func (r *memKBRepo) SetStatus(_ context.Context, id int64, status string, chunks int, errText *string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.files[id]
	if !ok {
		return repo.ErrNotFound
	}
	f.IndexStatus, f.ChunksCount, f.IndexError = status, chunks, errText
	return nil
}

func (r *memKBRepo) Delete(_ context.Context, id int64) (*models.EmmaKBFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.files[id]
	if !ok {
		return nil, repo.ErrNotFound
	}
	delete(r.files, id)
	return f, nil
}

// seed кладёт готовую строку (для list/reindex/delete-тестов).
func (r *memKBRepo) seed(f models.EmmaKBFile) models.EmmaKBFile {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	f.ID = r.nextID
	if f.CreatedAt.IsZero() {
		f.CreatedAt = time.Now().UTC()
	}
	r.files[f.ID] = &f
	return f
}

// fakeKBEnqueuer — записывает постановки emma:kb:index.
type fakeKBEnqueuer struct {
	mu    sync.Mutex
	calls []struct{ fileID, version int64 }
	err   error
}

func (e *fakeKBEnqueuer) EnqueueKBIndex(_ context.Context, fileID, version int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	e.calls = append(e.calls, struct{ fileID, version int64 }{fileID, version})
	return nil
}

func (e *fakeKBEnqueuer) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

func (e *fakeKBEnqueuer) last(t *testing.T) struct{ fileID, version int64 } {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.calls) == 0 {
		t.Fatal("emma:kb:index не поставлена")
	}
	return e.calls[len(e.calls)-1]
}

// --- хелперы ---

// uploadKB шлёт multipart POST /api/emma/kb с одним файлом в поле file.
func uploadKB(t *testing.T, rg *rig, token, filename string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/emma/kb", &body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	rg.router.ServeHTTP(w, req)
	return w
}

// kbAdmin — риг с открытой PIN-сессией (все kb-ручки за RequirePIN).
func kbAdmin(t *testing.T) (*rig, string) {
	t.Helper()
	rg := newRig(t, newFakeStore())
	admin := rg.token(t, auth.RoleAdmin)
	setupPIN(t, rg, admin, "123456")
	return rg, admin
}

// diskFiles — имена файлов в каталоге kb (оригиналы под UUID).
func diskFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// --- загрузка ---

func TestKBUploadTxt(t *testing.T) {
	rg, admin := kbAdmin(t)

	w := uploadKB(t, rg, admin, "факты.txt", []byte("Секретный код Интерфин: 998877."))
	m := wantStatus(t, w, http.StatusAccepted, "")

	if m["filename"] != "факты.txt" || m["status"] != models.EmmaKBPending {
		t.Fatalf("тело ответа: %v", m)
	}
	if m["id"].(float64) != 1 {
		t.Fatalf("id = %v, ждали 1", m["id"])
	}
	// Оригинал на диске под UUID (не под оригинальным именем — ТЗ §2.3).
	names := diskFiles(t, rg.kbDir)
	if len(names) != 1 || names[0] == "факты.txt" {
		t.Fatalf("каталог kb: %v", names)
	}
	// Задача поставлена на этот файл.
	if got := rg.kbEnq.last(t); got.fileID != 1 {
		t.Fatalf("enqueue file_id = %d", got.fileID)
	}
}

// Повторная загрузка того же filename — ТА ЖЕ строка (id стабилен),
// вытесненный оригинал удалён с диска (QA-фикс ТЗ §3).
func TestKBUploadRepeatSameFilenameKeepsID(t *testing.T) {
	rg, admin := kbAdmin(t)

	m1 := wantStatus(t, uploadKB(t, rg, admin, "prices.md", []byte("# v1")),
		http.StatusAccepted, "")
	m2 := wantStatus(t, uploadKB(t, rg, admin, "prices.md", []byte("# v2 длиннее")),
		http.StatusAccepted, "")

	if m1["id"] != m2["id"] {
		t.Fatalf("id сменился: %v → %v (ломает source panel:<id>)", m1["id"], m2["id"])
	}
	// Старый оригинал удалён — на диске ровно один файл (новый).
	if names := diskFiles(t, rg.kbDir); len(names) != 1 {
		t.Fatalf("на диске %d файлов, ждали 1: %v", len(names), names)
	}
	if rg.kbEnq.count() != 2 {
		t.Fatalf("постановок %d, ждали 2", rg.kbEnq.count())
	}
}

// Критерий приёмки: файл 51 МБ → 413 FILE_TOO_LARGE.
func TestKBUploadTooLarge413(t *testing.T) {
	rg, admin := kbAdmin(t)

	w := uploadKB(t, rg, admin, "big.txt", bytes.Repeat([]byte("a"), 51<<20))
	wantStatus(t, w, http.StatusRequestEntityTooLarge, CodeFileTooLarge)

	if rg.kbEnq.count() != 0 {
		t.Fatal("задача поставлена для отвергнутого файла")
	}
	if names := diskFiles(t, rg.kbDir); len(names) != 0 {
		t.Fatalf("отвергнутый файл остался на диске: %v", names)
	}
}

// Критерий приёмки: .docx и переименованный .exe → 400 FILE_TYPE_UNSUPPORTED
// (allowlist расширений + сигнатура содержимого, ТЗ §2.3).
func TestKBUploadUnsupportedType400(t *testing.T) {
	rg, admin := kbAdmin(t)

	cases := []struct {
		name     string
		filename string
		content  []byte
	}{
		{"docx вне allowlist", "report.docx", []byte("PK\x03\x04...")},
		{"exe вне allowlist", "tool.exe", []byte("MZ\x90\x00")},
		{"exe переименован в pdf: нет сигнатуры %PDF", "tool.pdf", []byte("MZ\x90\x00бинарь")},
		{"бинарь переименован в txt: не UTF-8", "tool.txt", []byte{0x4D, 0x5A, 0x90, 0x00, 0xFF, 0xFE}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := uploadKB(t, rg, admin, tc.filename, tc.content)
			wantStatus(t, w, http.StatusBadRequest, CodeFileTypeUnsupported)
		})
	}
	if rg.kbEnq.count() != 0 {
		t.Fatal("задача поставлена для отвергнутого файла")
	}
	if names := diskFiles(t, rg.kbDir); len(names) != 0 {
		t.Fatalf("отвергнутые файлы остались на диске: %v", names)
	}
}

// PDF с валидной сигнатурой проходит (содержимое дальше решает воркер).
func TestKBUploadPDFSignatureOK(t *testing.T) {
	rg, admin := kbAdmin(t)
	w := uploadKB(t, rg, admin, "прайс.pdf", []byte("%PDF-1.4\n..."))
	m := wantStatus(t, w, http.StatusAccepted, "")
	if m["mime"] != "application/pdf" {
		t.Fatalf("mime = %v", m["mime"])
	}
}

// --- список ---

func TestKBList(t *testing.T) {
	rg, admin := kbAdmin(t)
	errText := "PDF без текстового слоя, OCR не поддерживается"
	rg.kb.seed(models.EmmaKBFile{
		Filename: "a.txt", MimeType: "text/plain", FilePath: "x", FileSize: 10,
		IndexStatus: models.EmmaKBIndexed, ChunksCount: 3,
	})
	rg.kb.seed(models.EmmaKBFile{
		Filename: "b.pdf", MimeType: "application/pdf", FilePath: "y", FileSize: 20,
		IndexStatus: models.EmmaKBError, IndexError: &errText,
	})

	m := wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/kb", admin, nil),
		http.StatusOK, "")
	items := m["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("items: %d", len(items))
	}
	// Новые → старые: первым b.pdf (id 2).
	first := items[0].(map[string]interface{})
	if first["filename"] != "b.pdf" || first["status"] != models.EmmaKBError ||
		first["index_error"] != errText {
		t.Fatalf("первая строка: %v", first)
	}
	second := items[1].(map[string]interface{})
	if second["chunks_count"].(float64) != 3 {
		t.Fatalf("chunks_count: %v", second)
	}
}

// --- reindex ---

func TestKBReindex(t *testing.T) {
	rg, admin := kbAdmin(t)
	errText := "не вышло"
	f := rg.kb.seed(models.EmmaKBFile{
		Filename: "a.txt", MimeType: "text/plain", FilePath: "x", FileSize: 10,
		IndexStatus: models.EmmaKBError, IndexError: &errText, ChunksCount: 0,
	})

	m := wantStatus(t, rg.do(t, http.MethodPost,
		fmt.Sprintf("/api/emma/kb/%d/reindex", f.ID), admin, nil),
		http.StatusAccepted, "")
	if m["status"] != models.EmmaKBPending || m["index_error"] != nil {
		t.Fatalf("после reindex: %v", m)
	}
	got, err := rg.kb.GetByID(context.Background(), f.ID)
	if err != nil || got.IndexStatus != models.EmmaKBPending || got.IndexError != nil {
		t.Fatalf("строка после reindex: %+v, %v", got, err)
	}
	if last := rg.kbEnq.last(t); last.fileID != f.ID {
		t.Fatalf("enqueue file_id = %d", last.fileID)
	}
}

func TestKBReindexNotFound404(t *testing.T) {
	rg, admin := kbAdmin(t)
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/kb/99/reindex", admin, nil),
		http.StatusNotFound, codeNotFound)
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/kb/abc/reindex", admin, nil),
		http.StatusNotFound, codeNotFound)
}

// --- delete ---

func TestKBDelete(t *testing.T) {
	rg, admin := kbAdmin(t)
	// Настоящий файл на диске — DELETE обязан его стереть.
	path := filepath.Join(rg.kbDir, "uuid-on-disk")
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := rg.kb.seed(models.EmmaKBFile{
		Filename: "a.txt", MimeType: "text/plain", FilePath: path, FileSize: 4,
		IndexStatus: models.EmmaKBIndexed, ChunksCount: 1,
	})

	w := rg.do(t, http.MethodDelete, fmt.Sprintf("/api/emma/kb/%d", f.ID), admin, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("статус %d, ждали 204: %s", w.Code, w.Body.String())
	}
	if _, err := rg.kb.GetByID(context.Background(), f.ID); err == nil {
		t.Fatal("строка не удалена")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("оригинал остался на диске: %v", err)
	}
}

func TestKBDeleteNotFound404(t *testing.T) {
	rg, admin := kbAdmin(t)
	wantStatus(t, rg.do(t, http.MethodDelete, "/api/emma/kb/99", admin, nil),
		http.StatusNotFound, codeNotFound)
}

// Redis лёг при enqueue → 500, файл остаётся pending (восстановление —
// [Переиндексировать]), 202 не врём.
func TestKBUploadEnqueueDown500(t *testing.T) {
	rg, admin := kbAdmin(t)
	rg.kbEnq.err = errRedisDown

	w := uploadKB(t, rg, admin, "a.txt", []byte("текст"))
	wantStatus(t, w, http.StatusInternalServerError, codeInternal)

	f, err := rg.kb.GetByID(context.Background(), 1)
	if err != nil || f.IndexStatus != models.EmmaKBPending {
		t.Fatalf("строка после сбоя enqueue: %+v, %v", f, err)
	}
}
