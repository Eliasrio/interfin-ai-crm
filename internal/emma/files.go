// files.go — роуты /api/emma/files* (EP-04, ТЗ §3/§6): библиотека файлов,
// которые Эмма отправляет клиентам по маркеру {{file:N}}. Все ручки висят
// на защищённой группе (JWT + RequireRole(admin) + RequirePIN + Audit).
//
// Конвейер загрузки — образец kb.go (EP-03): MaxBytesReader 50 МБ → 413,
// allowlist расширений + сигнатура содержимого → 400, оригинал на диск
// <files_dir>/<uuid>. description обязателен: без подсказки «когда слать»
// Эмме не принять решение об отправке (ТЗ §3, вкладка 3).
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

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/interfin/interfin-ai-crm/internal/kbtext"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// Mime изображений библиотеки (PDF — kbtext.MimePDF). Worker ветвится по
// префиксу image/ (SendPhoto), поэтому констант ему не нужно.
const (
	mimeJPEG = "image/jpeg"
	mimePNG  = "image/png"
)

// maxSendFileBytes — 50 МБ: потолок Telegram Bot API на отправку (ТЗ §2.3),
// тот же лимит, что у базы знаний.
const maxSendFileBytes = 50 << 20

// sendFileMimeByExt — allowlist расширений (ТЗ §3: PDF/JPG/PNG; DOCX
// исключён решением владельца — редактируемые документы клиентам не отдаём).
var sendFileMimeByExt = map[string]string{
	".pdf":  kbtext.MimePDF,
	".jpg":  mimeJPEG,
	".jpeg": mimeJPEG,
	".png":  mimePNG,
}

// FilesDeps — зависимости роутов files*.
type FilesDeps struct {
	Files repo.EmmaSendFilesRepo
	Dir   string // <emma.data_dir>/files — создан при старте (config.EnsureDirs)
	Log   *slog.Logger
}

// FilesHandler — GET/POST files, PATCH/DELETE files/:id.
type FilesHandler struct {
	deps FilesDeps
}

func NewFiles(deps FilesDeps) *FilesHandler {
	return &FilesHandler{deps: deps}
}

// Register вешает роуты на защищённую группу /api/emma (контракт EP-01).
func (h *FilesHandler) Register(g gin.IRouter) {
	g.GET("/files", h.list)
	g.POST("/files", h.upload)
	g.PATCH("/files/:id", h.update)
	g.DELETE("/files/:id", h.delete)
}

// sendFileJSON — строка файла в ответах API — контракт фронта EP-07.
func sendFileJSON(f *models.EmmaSendFile) gin.H {
	return gin.H{
		"id":          f.ID,
		"name":        f.Name,
		"description": f.Description,
		"mime":        f.MimeType,
		"size":        f.FileSize,
		"is_active":   f.IsActive,
		"created_at":  f.CreatedAt,
	}
}

// GET /api/emma/files — все файлы, новые → старые.
func (h *FilesHandler) list(c *gin.Context) {
	files, err := h.deps.Files.List(c.Request.Context())
	if err != nil {
		h.deps.Log.Error("emma: files list", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	items := make([]gin.H, 0, len(files))
	for i := range files {
		items = append(items, sendFileJSON(&files[i]))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// POST /api/emma/files — multipart: name + description (оба обязательны) +
// file (PDF/JPG/PNG, ≤50 МБ, сигнатура) → 201.
func (h *FilesHandler) upload(c *gin.Context) {
	// Лимит на ВСЁ тело запроса до разбора multipart: 51 МБ умирают здесь
	// (в prod ещё раньше — nginx client_max_body_size 50m, ТЗ §2.3).
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxSendFileBytes)

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
	name := strings.TrimSpace(c.PostForm("name"))
	if name == "" {
		apiError(c, http.StatusBadRequest, "поле name обязательно", codeValidation)
		return
	}
	description := strings.TrimSpace(c.PostForm("description"))
	if description == "" {
		// Без описания Эмме не решить, когда отправлять файл (ТЗ §3).
		apiError(c, http.StatusBadRequest,
			"поле description обязательно: подсказка Эмме, когда отправлять файл",
			codeValidation)
		return
	}

	filename := filepath.Base(strings.TrimSpace(fh.Filename))
	mime, ok := sendFileMimeByExt[strings.ToLower(filepath.Ext(filename))]
	if !ok {
		apiError(c, http.StatusBadRequest,
			"поддерживаются только .pdf, .jpg и .png", CodeFileTypeUnsupported)
		return
	}

	src, err := fh.Open()
	if err != nil {
		h.deps.Log.Error("emma: files upload: open multipart", "error", err)
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
		h.deps.Log.Error("emma: files upload: read multipart", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}

	// Сигнатура, не только расширение (ТЗ §2.3): переименованный .exe не
	// пройдёт ни как PDF, ни как JPEG/PNG.
	if !sendFileSignatureOK(mime, data) {
		apiError(c, http.StatusBadRequest,
			"содержимое файла не соответствует типу", CodeFileTypeUnsupported)
		return
	}

	// Оригинал на диск под UUID (ТЗ §2.3: имя на диске — UUID, оригинальное
	// имя вообще не сохраняется — клиент увидит name из панели).
	path := filepath.Join(h.deps.Dir, uuid.NewString())
	if err := os.WriteFile(path, data, 0o644); err != nil {
		h.deps.Log.Error("emma: files upload: запись на диск", "path", path, "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}

	f := &models.EmmaSendFile{
		Name:        name,
		Description: description,
		FilePath:    path,
		MimeType:    mime,
		FileSize:    int64(len(data)),
		IsActive:    true, // явно: zero value затёр бы DEFAULT TRUE (грабля M5)
	}
	if err := h.deps.Files.Create(c.Request.Context(), f); err != nil {
		// Строки нет — подчищаем свежезаписанный оригинал, не копим сирот.
		if rmErr := os.Remove(path); rmErr != nil {
			h.deps.Log.Warn("emma: files upload: сирота не удалена", "path", path, "error", rmErr)
		}
		h.deps.Log.Error("emma: files upload: create", "name", name, "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}

	h.deps.Log.Info("emma: файл для отправки загружен",
		"file_id", f.ID, "name", name, "mime", mime, "size", f.FileSize)
	c.JSON(http.StatusCreated, sendFileJSON(f))
}

// patchSendFileReq — тело PATCH: nil-поле не меняется (ТЗ §6).
type patchSendFileReq struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	IsActive    *bool   `json:"is_active"`
}

// PATCH /api/emma/files/:id — name/description/is_active.
func (h *FilesHandler) update(c *gin.Context) {
	id, ok := h.idParam(c)
	if !ok {
		return
	}
	var req patchSendFileReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiError(c, http.StatusBadRequest, "тело запроса не разобрано", codeValidation)
		return
	}
	if req.Name == nil && req.Description == nil && req.IsActive == nil {
		apiError(c, http.StatusBadRequest,
			"нужно хотя бы одно из полей name/description/is_active", codeValidation)
		return
	}
	upd := repo.EmmaSendFileUpdate{IsActive: req.IsActive}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			apiError(c, http.StatusBadRequest, "name не может быть пустым", codeValidation)
			return
		}
		upd.Name = &name
	}
	if req.Description != nil {
		description := strings.TrimSpace(*req.Description)
		if description == "" {
			apiError(c, http.StatusBadRequest,
				"description не может быть пустым: подсказка Эмме, когда отправлять файл",
				codeValidation)
			return
		}
		upd.Description = &description
	}

	f, err := h.deps.Files.Update(c.Request.Context(), id, upd)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		apiError(c, http.StatusNotFound, "файл не найден", codeNotFound)
		return
	case err != nil:
		h.deps.Log.Error("emma: files update", "file_id", id, "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	h.deps.Log.Info("emma: файл для отправки изменён",
		"file_id", f.ID, "is_active", f.IsActive)
	c.JSON(http.StatusOK, sendFileJSON(f))
}

// DELETE /api/emma/files/:id — строка + файл с диска (после коммита,
// best-effort); emma_events.send_file_id обнуляется БД (SET NULL, 0020).
func (h *FilesHandler) delete(c *gin.Context) {
	id, ok := h.idParam(c)
	if !ok {
		return
	}
	deleted, err := h.deps.Files.Delete(c.Request.Context(), id)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		apiError(c, http.StatusNotFound, "файл не найден", codeNotFound)
		return
	case err != nil:
		h.deps.Log.Error("emma: files delete", "file_id", id, "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	if err := os.Remove(deleted.FilePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		h.deps.Log.Warn("emma: files delete: оригинал не удалён с диска",
			"path", deleted.FilePath, "error", err)
	}
	h.deps.Log.Info("emma: файл для отправки удалён",
		"file_id", deleted.ID, "name", deleted.Name)
	c.Status(http.StatusNoContent)
}

// idParam — :id из пути; нечисловой и несуществующий id наружу не
// различаются — 404 (дисциплина fileByParam EP-03). false — ответ отправлен.
func (h *FilesHandler) idParam(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		apiError(c, http.StatusNotFound, "файл не найден", codeNotFound)
		return 0, false
	}
	return id, true
}

// sendFileSignatureOK — сигнатура содержимого соответствует заявленному
// типу: %PDF, JPEG FF D8 FF, PNG 89 50 4E 47 (ТЗ §2.3, task EP-04).
func sendFileSignatureOK(mime string, data []byte) bool {
	switch mime {
	case kbtext.MimePDF:
		return bytes.HasPrefix(data, []byte("%PDF"))
	case mimeJPEG:
		return bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF})
	case mimePNG:
		return bytes.HasPrefix(data, []byte{0x89, 0x50, 0x4E, 0x47})
	default:
		return false
	}
}
