// kb.go — роуты /api/emma/kb* (EP-03, ТЗ §3/§6): загрузка файлов базы
// знаний, список со статусами индексации, reindex, удаление. Все ручки
// висят на защищённой группе (JWT + RequireRole(admin) + RequirePIN +
// Audit) — про PIN ничего не знают.
//
// Конвейер загрузки: оригинал на диск <kb_dir>/<uuid> (вне web-root,
// ТЗ §2.3) → upsert строки по filename (повторная загрузка = ТА ЖЕ строка,
// id стабилен — QA-фикс ТЗ §3) → enqueue emma:kb:index → 202. Индексация
// строго асинхронная: Voyage rate-limit, большой файл — минуты; готовность
// приходит WS-событием emma_kb_status.
package emma

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/interfin/interfin-ai-crm/internal/kbtext"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// Коды ошибок контура базы знаний — контракт для фронта EP-07.
const (
	// CodeFileTooLarge — 413: файл больше лимита 50 МБ (ТЗ §2.3 — это же
	// потолок Telegram Bot API; в prod тот же лимит держит nginx).
	CodeFileTooLarge = "FILE_TOO_LARGE"
	// CodeFileTypeUnsupported — 400: расширение вне allowlist TXT/MD/PDF
	// или содержимое не соответствует типу (сигнатура, ТЗ §2.3).
	CodeFileTypeUnsupported = "FILE_TYPE_UNSUPPORTED"
)

// maxKBUploadBytes — 50 МБ (ТЗ §2.3).
const maxKBUploadBytes = 50 << 20

// kbMimeByExt — allowlist расширений и их mime для emma_kb_files.
var kbMimeByExt = map[string]string{
	".txt": kbtext.MimeTXT,
	".md":  kbtext.MimeMD,
	".pdf": kbtext.MimePDF,
}

// KBDeps — зависимости роутов kb*.
type KBDeps struct {
	Files repo.EmmaKBRepo
	Enq   queue.KBIndexEnqueuer
	KBDir string // <emma.data_dir>/kb — создан при старте (config.EnsureDirs)
	Log   *slog.Logger
}

// KBHandler — GET/POST kb, POST kb/:id/reindex, DELETE kb/:id.
type KBHandler struct {
	deps KBDeps
}

func NewKB(deps KBDeps) *KBHandler {
	return &KBHandler{deps: deps}
}

// Register вешает роуты на защищённую группу /api/emma (контракт EP-01).
func (h *KBHandler) Register(g gin.IRouter) {
	g.GET("/kb", h.list)
	g.POST("/kb", h.upload)
	g.POST("/kb/:id/reindex", h.reindex)
	g.DELETE("/kb/:id", h.delete)
}

// kbFileJSON — строка файла в ответах API (список, upload, reindex) —
// контракт фронта EP-07 (task §7).
func kbFileJSON(f *models.EmmaKBFile) gin.H {
	return gin.H{
		"id":           f.ID,
		"filename":     f.Filename,
		"mime":         f.MimeType,
		"size":         f.FileSize,
		"status":       f.IndexStatus,
		"chunks_count": f.ChunksCount,
		"index_error":  f.IndexError,
		"created_at":   f.CreatedAt,
	}
}

// GET /api/emma/kb — все файлы, новые → старые.
func (h *KBHandler) list(c *gin.Context) {
	files, err := h.deps.Files.List(c.Request.Context())
	if err != nil {
		h.deps.Log.Error("emma: kb list", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	items := make([]gin.H, 0, len(files))
	for i := range files {
		items = append(items, kbFileJSON(&files[i]))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// POST /api/emma/kb — multipart-загрузка (поле file). 202: строка создана/
// заменена, индексация поставлена в очередь (фронт ждёт WS emma_kb_status).
func (h *KBHandler) upload(c *gin.Context) {
	// Лимит на ВСЁ тело запроса до разбора multipart: 51 МБ умирают здесь
	// (в prod ещё раньше — nginx client_max_body_size 50m, ТЗ §2.3).
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxKBUploadBytes)

	fh, err := c.FormFile("file")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			apiError(c, http.StatusRequestEntityTooLarge,
				"файл больше 50 МБ", CodeFileTooLarge)
			return
		}
		apiError(c, http.StatusBadRequest,
			"multipart-поле file не разобрано", codeValidation)
		return
	}

	filename := filepath.Base(strings.TrimSpace(fh.Filename))
	if filename == "" || filename == "." || filename == string(filepath.Separator) {
		apiError(c, http.StatusBadRequest, "пустое имя файла", codeValidation)
		return
	}
	mime, ok := kbMimeByExt[strings.ToLower(filepath.Ext(filename))]
	if !ok {
		apiError(c, http.StatusBadRequest,
			"поддерживаются только .txt, .md и .pdf", CodeFileTypeUnsupported)
		return
	}

	src, err := fh.Open()
	if err != nil {
		h.deps.Log.Error("emma: kb upload: open multipart", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	defer src.Close() //nolint:errcheck // только чтение
	data, err := io.ReadAll(src)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			apiError(c, http.StatusRequestEntityTooLarge,
				"файл больше 50 МБ", CodeFileTooLarge)
			return
		}
		h.deps.Log.Error("emma: kb upload: read multipart", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}

	// Сигнатура, не только расширение (ТЗ §2.3): переименованный .exe
	// не пройдёт ни как PDF (%PDF), ни как текст (валидный UTF-8).
	if !kbSignatureOK(mime, data) {
		apiError(c, http.StatusBadRequest,
			"содержимое файла не соответствует типу", CodeFileTypeUnsupported)
		return
	}

	// Оригинал на диск под UUID (ТЗ §2.3: имя на диске — UUID, оригинальное
	// имя — только в БД; никаких путей из пользовательского ввода).
	path := filepath.Join(h.deps.KBDir, uuid.NewString())
	if err := os.WriteFile(path, data, 0o644); err != nil {
		h.deps.Log.Error("emma: kb upload: запись на диск", "path", path, "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}

	f := &models.EmmaKBFile{
		Filename: filename,
		MimeType: mime,
		FilePath: path,
		FileSize: int64(len(data)),
	}
	ctx := c.Request.Context()
	oldPath, err := h.deps.Files.UpsertByFilename(ctx, f)
	if err != nil {
		// Строки нет — подчищаем свежезаписанный оригинал, не копим сирот.
		if rmErr := os.Remove(path); rmErr != nil {
			h.deps.Log.Warn("emma: kb upload: сирота не удалена", "path", path, "error", rmErr)
		}
		h.deps.Log.Error("emma: kb upload: upsert", "filename", filename, "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	// Повторная загрузка вытеснила старый оригинал — удалить с диска
	// (best-effort: файл-сирота хуже строки-сироты не станет).
	if oldPath != "" && oldPath != path {
		if err := os.Remove(oldPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			h.deps.Log.Warn("emma: kb upload: старый оригинал не удалён",
				"path", oldPath, "error", err)
		}
	}

	if !h.enqueue(c, f.ID) {
		return
	}
	h.deps.Log.Info("emma: kb-файл загружен",
		"file_id", f.ID, "filename", filename, "size", f.FileSize, "replaced", oldPath != "")
	c.JSON(http.StatusAccepted, kbFileJSON(f))
}

// POST /api/emma/kb/:id/reindex — status pending + новая задача индексации
// (текст заново извлекается из оригинала на диске). 404 — нет id.
func (h *KBHandler) reindex(c *gin.Context) {
	f, ok := h.fileByParam(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	if err := h.deps.Files.SetStatus(ctx, f.ID, models.EmmaKBPending, 0, nil); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			apiError(c, http.StatusNotFound, "файл не найден", codeNotFound)
			return
		}
		h.deps.Log.Error("emma: kb reindex: статус", "file_id", f.ID, "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	f.IndexStatus = models.EmmaKBPending
	f.ChunksCount = 0
	f.IndexError = nil

	if !h.enqueue(c, f.ID) {
		return
	}
	h.deps.Log.Info("emma: kb-файл поставлен на переиндексацию", "file_id", f.ID)
	c.JSON(http.StatusAccepted, kbFileJSON(f))
}

// DELETE /api/emma/kb/:id — транзакция «чанки source + строка», после
// коммита — оригинал с диска (best-effort, ТЗ §3). 404 — нет id.
func (h *KBHandler) delete(c *gin.Context) {
	f, ok := h.fileByParam(c)
	if !ok {
		return
	}
	deleted, err := h.deps.Files.Delete(c.Request.Context(), f.ID)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		apiError(c, http.StatusNotFound, "файл не найден", codeNotFound)
		return
	case err != nil:
		h.deps.Log.Error("emma: kb delete", "file_id", f.ID, "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	if err := os.Remove(deleted.FilePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		h.deps.Log.Warn("emma: kb delete: оригинал не удалён с диска",
			"path", deleted.FilePath, "error", err)
	}
	h.deps.Log.Info("emma: kb-файл удалён",
		"file_id", deleted.ID, "filename", deleted.Filename)
	c.Status(http.StatusNoContent)
}

// enqueue — постановка emma:kb:index. Версия TaskID — unix-время сейчас
// (upload/reindex = новая версия; unique-замок старой задачи не мешает —
// грабля M3). ErrDuplicate (двойной сабмит в ту же секунду) — не сбой.
// false — ответ уже отправлен (500).
func (h *KBHandler) enqueue(c *gin.Context, fileID int64) bool {
	err := h.deps.Enq.EnqueueKBIndex(c.Request.Context(), fileID, time.Now().Unix())
	switch {
	case errors.Is(err, queue.ErrDuplicate):
		h.deps.Log.Info("emma: kb index уже в очереди", "file_id", fileID)
		return true
	case err != nil:
		// Строка останется pending без задачи — владелец повторит через
		// [Переиндексировать]; врать 202 при мёртвом Redis хуже.
		h.deps.Log.Error("emma: kb enqueue", "file_id", fileID, "error", err)
		apiError(c, http.StatusInternalServerError,
			"индексация не поставлена в очередь", codeInternal)
		return false
	}
	return true
}

// fileByParam — файл по :id; нечисловой и несуществующий id наружу не
// различаются — 404 (как versionByParam EP-02). false — ответ отправлен.
func (h *KBHandler) fileByParam(c *gin.Context) (*models.EmmaKBFile, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		apiError(c, http.StatusNotFound, "файл не найден", codeNotFound)
		return nil, false
	}
	f, err := h.deps.Files.GetByID(c.Request.Context(), id)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		apiError(c, http.StatusNotFound, "файл не найден", codeNotFound)
		return nil, false
	case err != nil:
		h.deps.Log.Error("emma: kb get", "file_id", id, "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return nil, false
	}
	return f, true
}

// kbSignatureOK — сигнатура содержимого соответствует заявленному типу:
// PDF начинается с %PDF, текст — валидный UTF-8 (ТЗ §2.3).
func kbSignatureOK(mime string, data []byte) bool {
	switch mime {
	case kbtext.MimePDF:
		return bytes.HasPrefix(data, []byte("%PDF"))
	default:
		return utf8.Valid(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	}
}
