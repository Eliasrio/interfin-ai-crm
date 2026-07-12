// Тесты роутов /api/emma/files* (EP-04): обязательные name/description → 400,
// лимит 50 МБ → 413, allowlist + сигнатуры → 400, PATCH/DELETE (файл с диска).
// Гейты (manager → 403, без PIN → 401) — общие тесты emma_test.go,
// files-роуты включены в emmaPaths.
package emma

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// --- фейки EP-04 ---

// memSendFilesRepo — in-memory repo.EmmaSendFilesRepo.
type memSendFilesRepo struct {
	mu     sync.Mutex
	nextID int64
	files  map[int64]*models.EmmaSendFile
}

func newMemSendFilesRepo() *memSendFilesRepo {
	return &memSendFilesRepo{files: map[int64]*models.EmmaSendFile{}}
}

func (r *memSendFilesRepo) List(context.Context) ([]models.EmmaSendFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]models.EmmaSendFile, 0, len(r.files))
	for _, f := range r.files {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (r *memSendFilesRepo) GetByID(_ context.Context, id int64) (*models.EmmaSendFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.files[id]
	if !ok {
		return nil, repo.ErrNotFound
	}
	cp := *f
	return &cp, nil
}

func (r *memSendFilesRepo) Create(_ context.Context, f *models.EmmaSendFile) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	f.ID = r.nextID
	f.CreatedAt = time.Now().UTC()
	cp := *f
	r.files[f.ID] = &cp
	return nil
}

func (r *memSendFilesRepo) Update(_ context.Context, id int64, upd repo.EmmaSendFileUpdate) (*models.EmmaSendFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.files[id]
	if !ok {
		return nil, repo.ErrNotFound
	}
	if upd.Name != nil {
		f.Name = *upd.Name
	}
	if upd.Description != nil {
		f.Description = *upd.Description
	}
	if upd.IsActive != nil {
		f.IsActive = *upd.IsActive
	}
	cp := *f
	return &cp, nil
}

func (r *memSendFilesRepo) Delete(_ context.Context, id int64) (*models.EmmaSendFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.files[id]
	if !ok {
		return nil, repo.ErrNotFound
	}
	delete(r.files, id)
	return f, nil
}

func (r *memSendFilesRepo) ListActive(context.Context) ([]models.EmmaSendFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]models.EmmaSendFile, 0, len(r.files))
	for _, f := range r.files {
		if f.IsActive {
			out = append(out, *f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// --- хелперы ---

// Валидные сигнатуры содержимого (task EP-04).
var (
	pdfBytes  = []byte("%PDF-1.7 fake")
	jpegBytes = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("jfif")...)
	pngBytes  = append([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, []byte("idat")...)
)

// uploadSendFile шлёт multipart POST /api/emma/files (name/description/file).
// Пустая строка поля — поле не отправляется (проверка «обязателен»).
func uploadSendFile(t *testing.T, rg *rig, token, name, description, filename string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if name != "" {
		if err := mw.WriteField("name", name); err != nil {
			t.Fatal(err)
		}
	}
	if description != "" {
		if err := mw.WriteField("description", description); err != nil {
			t.Fatal(err)
		}
	}
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
	req := httptest.NewRequest(http.MethodPost, "/api/emma/files", &body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	rg.router.ServeHTTP(w, req)
	return w
}

// filesAdmin — риг с открытой PIN-сессией (все files-ручки за RequirePIN).
func filesAdmin(t *testing.T) (*rig, string) {
	t.Helper()
	rg := newRig(t, newFakeStore())
	admin := rg.token(t, auth.RoleAdmin)
	setupPIN(t, rg, admin, "123456")
	return rg, admin
}

// --- загрузка ---

func TestSendFileUploadPDF(t *testing.T) {
	rg, admin := filesAdmin(t)

	w := uploadSendFile(t, rg, admin, "Прайс 2026",
		"отправь, когда клиент спрашивает подробные цены", "прайс.pdf", pdfBytes)
	m := wantStatus(t, w, http.StatusCreated, "")

	if m["name"] != "Прайс 2026" || m["mime"] != "application/pdf" {
		t.Fatalf("тело ответа: %v", m)
	}
	if active, _ := m["is_active"].(bool); !active {
		t.Fatalf("новый файл обязан быть активным: %v", m)
	}
	// Оригинал на диске под UUID (не под оригинальным именем — ТЗ §2.3).
	names := diskFiles(t, rg.filesDir)
	if len(names) != 1 || names[0] == "прайс.pdf" {
		t.Fatalf("каталог files: %v", names)
	}
	// Строка в репозитории с путём к этому оригиналу.
	f, err := rg.sendFiles.GetByID(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if f.FilePath != filepath.Join(rg.filesDir, names[0]) {
		t.Fatalf("file_path %q не указывает на оригинал %q", f.FilePath, names[0])
	}
}

func TestSendFileUploadImages(t *testing.T) {
	rg, admin := filesAdmin(t)

	wantStatus(t, uploadSendFile(t, rg, admin, "Фото офиса", "покажи офис",
		"office.jpg", jpegBytes), http.StatusCreated, "")
	wantStatus(t, uploadSendFile(t, rg, admin, "Схема проезда", "как добраться",
		"map.PNG", pngBytes), http.StatusCreated, "")
}

// Критерий приёмки EP-04: POST без description → 400.
func TestSendFileUploadRequiresNameAndDescription(t *testing.T) {
	rg, admin := filesAdmin(t)

	wantStatus(t, uploadSendFile(t, rg, admin, "Прайс", "", "прайс.pdf", pdfBytes),
		http.StatusBadRequest, codeValidation)
	wantStatus(t, uploadSendFile(t, rg, admin, "", "когда спросят", "прайс.pdf", pdfBytes),
		http.StatusBadRequest, codeValidation)
	// Ничего не создано и на диске пусто.
	if names := diskFiles(t, rg.filesDir); len(names) != 0 {
		t.Fatalf("каталог files не пуст: %v", names)
	}
}

// Критерий приёмки EP-04: .docx/.exe → 400 (DOCX исключён решением владельца).
func TestSendFileUploadRejectsUnsupportedTypes(t *testing.T) {
	rg, admin := filesAdmin(t)

	wantStatus(t, uploadSendFile(t, rg, admin, "Договор", "шаблон договора",
		"договор.docx", []byte("PK\x03\x04zip")), http.StatusBadRequest, CodeFileTypeUnsupported)
	wantStatus(t, uploadSendFile(t, rg, admin, "Вирус", "не надо",
		"tool.exe", []byte("MZ\x90\x00")), http.StatusBadRequest, CodeFileTypeUnsupported)
	// Переименованный .exe не проходит по сигнатуре ни в один тип.
	wantStatus(t, uploadSendFile(t, rg, admin, "Хитрый", "переименован",
		"тихий.pdf", []byte("MZ\x90\x00")), http.StatusBadRequest, CodeFileTypeUnsupported)
	wantStatus(t, uploadSendFile(t, rg, admin, "Хитрый2", "переименован",
		"тихий.jpg", []byte("MZ\x90\x00")), http.StatusBadRequest, CodeFileTypeUnsupported)
}

// Критерий приёмки EP-04: 51 МБ → 413.
func TestSendFileUploadTooLarge(t *testing.T) {
	rg, admin := filesAdmin(t)

	big := make([]byte, 51<<20)
	copy(big, pdfBytes)
	wantStatus(t, uploadSendFile(t, rg, admin, "Огромный", "не влезет", "big.pdf", big),
		http.StatusRequestEntityTooLarge, CodeFileTooLarge)
}

// --- список ---

func TestSendFilesList(t *testing.T) {
	rg, admin := filesAdmin(t)
	wantStatus(t, uploadSendFile(t, rg, admin, "Прайс", "цены", "p.pdf", pdfBytes),
		http.StatusCreated, "")

	m := wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/files", admin, nil),
		http.StatusOK, "")
	items, _ := m["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items: %v", m)
	}
	row := items[0].(map[string]any)
	for _, key := range []string{"id", "name", "description", "mime", "size", "is_active", "created_at"} {
		if _, ok := row[key]; !ok {
			t.Errorf("в строке списка нет поля %q: %v", key, row)
		}
	}
}

// --- PATCH ---

func TestSendFilePatch(t *testing.T) {
	rg, admin := filesAdmin(t)
	wantStatus(t, uploadSendFile(t, rg, admin, "Прайс", "цены", "p.pdf", pdfBytes),
		http.StatusCreated, "")

	// Частичное обновление: только is_active (критерий приёмки: выключение).
	m := wantStatus(t, rg.do(t, http.MethodPatch, "/api/emma/files/1", admin,
		map[string]any{"is_active": false}), http.StatusOK, "")
	if m["is_active"].(bool) || m["name"] != "Прайс" {
		t.Fatalf("после PATCH is_active: %v", m)
	}
	// Выключенный файл пропал из ListActive (источник секции промпта).
	active, err := rg.sendFiles.ListActive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("выключенный файл остался в ListActive: %v", active)
	}

	// name + description разом.
	m = wantStatus(t, rg.do(t, http.MethodPatch, "/api/emma/files/1", admin,
		map[string]any{"name": "Прайс 2027", "description": "новые цены"}), http.StatusOK, "")
	if m["name"] != "Прайс 2027" || m["description"] != "новые цены" {
		t.Fatalf("после PATCH name/description: %v", m)
	}

	// Пустое тело — нечего менять → 400; пустой name → 400.
	wantStatus(t, rg.do(t, http.MethodPatch, "/api/emma/files/1", admin,
		map[string]any{}), http.StatusBadRequest, codeValidation)
	wantStatus(t, rg.do(t, http.MethodPatch, "/api/emma/files/1", admin,
		map[string]any{"name": "  "}), http.StatusBadRequest, codeValidation)
	// Несуществующий id → 404.
	wantStatus(t, rg.do(t, http.MethodPatch, "/api/emma/files/99", admin,
		map[string]any{"is_active": true}), http.StatusNotFound, codeNotFound)
}

// --- DELETE ---

func TestSendFileDelete(t *testing.T) {
	rg, admin := filesAdmin(t)
	wantStatus(t, uploadSendFile(t, rg, admin, "Прайс", "цены", "p.pdf", pdfBytes),
		http.StatusCreated, "")
	if names := diskFiles(t, rg.filesDir); len(names) != 1 {
		t.Fatalf("каталог files: %v", names)
	}

	w := rg.do(t, http.MethodDelete, "/api/emma/files/1", admin, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d, тело: %s", w.Code, w.Body.String())
	}
	// Строки нет и файла на диске нет (критерий приёмки EP-04).
	if _, err := rg.sendFiles.GetByID(context.Background(), 1); err == nil {
		t.Fatal("строка файла пережила DELETE")
	}
	if names := diskFiles(t, rg.filesDir); len(names) != 0 {
		t.Fatalf("оригинал пережил DELETE: %v", names)
	}
	// Повторный DELETE → 404.
	wantStatus(t, rg.do(t, http.MethodDelete, "/api/emma/files/1", admin, nil),
		http.StatusNotFound, codeNotFound)
}

// Санити: mime-константы согласованы с ветвлением воркера (image/ → фото).
func TestSendFileMimeMap(t *testing.T) {
	for ext, mime := range sendFileMimeByExt {
		if ext == ".pdf" {
			if mime != "application/pdf" {
				t.Errorf("%s → %s", ext, mime)
			}
			continue
		}
		if !bytes.HasPrefix([]byte(mime), []byte("image/")) {
			t.Errorf("%s → %s: не image/*, воркер отправит документом", ext, mime)
		}
	}
	// Защита от опечатки в allowlist: ровно 4 расширения ТЗ §3.
	if len(sendFileMimeByExt) != 4 {
		t.Errorf("allowlist: %v", sendFileMimeByExt)
	}
}
