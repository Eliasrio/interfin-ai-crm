// contacts.go — роуты /api/emma/contacts* (EP-05, ТЗ §3/§6): справочник
// контактов и ссылок, попадающий в system-блок Эммы (только активные).
// Все ручки висят на защищённой группе (JWT + RequireRole(admin) +
// RequirePIN + Audit).
//
// Лимит 30 активных охраняет репозиторий (транзакция + advisory-лок —
// гонка двух PATCH не даёт 31-го); здесь ErrContactsLimit мапится в
// 400 {"code":"CONTACTS_LIMIT","limit":30} — контракт фронта EP-07.
package emma

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// CodeContactsLimit — 400: попытка включить 31-й активный контакт (ТЗ §3).
const CodeContactsLimit = "CONTACTS_LIMIT"

// contactTypes — допустимые значения type (зеркало CHECK миграции 0019).
var contactTypes = map[string]bool{
	"phone":    true,
	"whatsapp": true,
	"telegram": true,
	"email":    true,
	"website":  true,
	"other":    true,
}

// ContactsDeps — зависимости роутов contacts*.
type ContactsDeps struct {
	Contacts repo.EmmaContactsRepo
	Log      *slog.Logger
}

// ContactsHandler — GET/POST contacts, PATCH/DELETE contacts/:id.
type ContactsHandler struct {
	deps ContactsDeps
}

func NewContacts(deps ContactsDeps) *ContactsHandler {
	return &ContactsHandler{deps: deps}
}

// Register вешает роуты на защищённую группу /api/emma (контракт EP-01).
func (h *ContactsHandler) Register(g gin.IRouter) {
	g.GET("/contacts", h.list)
	g.POST("/contacts", h.create)
	g.PATCH("/contacts/:id", h.update)
	g.DELETE("/contacts/:id", h.delete)
}

// contactJSON — строка контакта в ответах API — контракт фронта EP-07.
func contactJSON(c *models.EmmaContact) gin.H {
	comment := ""
	if c.Comment != nil {
		comment = *c.Comment
	}
	return gin.H{
		"id":         c.ID,
		"type":       c.Type,
		"name":       c.Name,
		"value":      c.Value,
		"comment":    comment,
		"is_active":  c.IsActive,
		"sort_order": c.SortOrder,
	}
}

// GET /api/emma/contacts — все контакты (sort_order, новые в конец) +
// limit для счётчика вкладки 4.
func (h *ContactsHandler) list(c *gin.Context) {
	contacts, err := h.deps.Contacts.List(c.Request.Context())
	if err != nil {
		h.deps.Log.Error("emma: contacts list", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	items := make([]gin.H, 0, len(contacts))
	for i := range contacts {
		items = append(items, contactJSON(&contacts[i]))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "limit": repo.MaxActiveContacts})
}

// createContactReq — тело POST. is_active по умолчанию true, sort_order 0.
type createContactReq struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Value     string `json:"value"`
	Comment   string `json:"comment"`
	IsActive  *bool  `json:"is_active"`
	SortOrder *int   `json:"sort_order"`
}

// POST /api/emma/contacts → 201; 400 CONTACTS_LIMIT при 30 активных.
func (h *ContactsHandler) create(c *gin.Context) {
	var req createContactReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiError(c, http.StatusBadRequest, "тело запроса не разобрано", codeValidation)
		return
	}
	req.Type = strings.TrimSpace(req.Type)
	req.Name = strings.TrimSpace(req.Name)
	req.Value = strings.TrimSpace(req.Value)
	if !contactTypes[req.Type] {
		apiError(c, http.StatusBadRequest,
			"type должен быть одним из: phone, whatsapp, telegram, email, website, other",
			codeValidation)
		return
	}
	if req.Name == "" || req.Value == "" {
		apiError(c, http.StatusBadRequest, "поля name и value обязательны", codeValidation)
		return
	}

	contact := &models.EmmaContact{
		Type:     req.Type,
		Name:     req.Name,
		Value:    req.Value,
		IsActive: true, // явно: zero value затёр бы DEFAULT TRUE (грабля M5)
	}
	if comment := strings.TrimSpace(req.Comment); comment != "" {
		contact.Comment = &comment
	}
	if req.IsActive != nil {
		contact.IsActive = *req.IsActive
	}
	if req.SortOrder != nil {
		contact.SortOrder = *req.SortOrder
	}

	err := h.deps.Contacts.Create(c.Request.Context(), contact)
	switch {
	case errors.Is(err, repo.ErrContactsLimit):
		contactsLimitError(c)
		return
	case err != nil:
		h.deps.Log.Error("emma: contacts create", "name", req.Name, "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	h.deps.Log.Info("emma: контакт создан",
		"contact_id", contact.ID, "type", contact.Type, "name", contact.Name)
	c.JSON(http.StatusCreated, contactJSON(contact))
}

// patchContactReq — тело PATCH: nil-поле не меняется (ТЗ §6).
type patchContactReq struct {
	Type      *string `json:"type"`
	Name      *string `json:"name"`
	Value     *string `json:"value"`
	Comment   *string `json:"comment"` // "" — стереть комментарий
	IsActive  *bool   `json:"is_active"`
	SortOrder *int    `json:"sort_order"`
}

// PATCH /api/emma/contacts/:id — все поля + is_active (ТЗ §6);
// включение при 30 активных → 400 CONTACTS_LIMIT.
func (h *ContactsHandler) update(c *gin.Context) {
	id, ok := contactID(c)
	if !ok {
		return
	}
	var req patchContactReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiError(c, http.StatusBadRequest, "тело запроса не разобрано", codeValidation)
		return
	}
	if req.Type == nil && req.Name == nil && req.Value == nil &&
		req.Comment == nil && req.IsActive == nil && req.SortOrder == nil {
		apiError(c, http.StatusBadRequest, "нужно хотя бы одно поле", codeValidation)
		return
	}
	upd := repo.EmmaContactUpdate{IsActive: req.IsActive, SortOrder: req.SortOrder}
	if req.Type != nil {
		typ := strings.TrimSpace(*req.Type)
		if !contactTypes[typ] {
			apiError(c, http.StatusBadRequest,
				"type должен быть одним из: phone, whatsapp, telegram, email, website, other",
				codeValidation)
			return
		}
		upd.Type = &typ
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			apiError(c, http.StatusBadRequest, "name не может быть пустым", codeValidation)
			return
		}
		upd.Name = &name
	}
	if req.Value != nil {
		value := strings.TrimSpace(*req.Value)
		if value == "" {
			apiError(c, http.StatusBadRequest, "value не может быть пустым", codeValidation)
			return
		}
		upd.Value = &value
	}
	if req.Comment != nil {
		comment := strings.TrimSpace(*req.Comment)
		upd.Comment = &comment // "" легально — репозиторий сбросит в NULL
	}

	contact, err := h.deps.Contacts.Update(c.Request.Context(), id, upd)
	switch {
	case errors.Is(err, repo.ErrContactsLimit):
		contactsLimitError(c)
		return
	case errors.Is(err, repo.ErrNotFound):
		apiError(c, http.StatusNotFound, "контакт не найден", codeNotFound)
		return
	case err != nil:
		h.deps.Log.Error("emma: contacts update", "contact_id", id, "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	h.deps.Log.Info("emma: контакт изменён",
		"contact_id", contact.ID, "is_active", contact.IsActive)
	c.JSON(http.StatusOK, contactJSON(contact))
}

// DELETE /api/emma/contacts/:id → 204.
func (h *ContactsHandler) delete(c *gin.Context) {
	id, ok := contactID(c)
	if !ok {
		return
	}
	err := h.deps.Contacts.Delete(c.Request.Context(), id)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		apiError(c, http.StatusNotFound, "контакт не найден", codeNotFound)
		return
	case err != nil:
		h.deps.Log.Error("emma: contacts delete", "contact_id", id, "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	h.deps.Log.Info("emma: контакт удалён", "contact_id", id)
	c.Status(http.StatusNoContent)
}

// contactID — :id из пути; нечисловой и несуществующий id наружу не
// различаются — 404 (дисциплина idParam EP-04). false — ответ отправлен.
func contactID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		apiError(c, http.StatusNotFound, "контакт не найден", codeNotFound)
		return 0, false
	}
	return id, true
}

// contactsLimitError — 400 CONTACTS_LIMIT с полем limit (task EP-05 §1).
func contactsLimitError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
		"error": "лимит активных контактов исчерпан: выключите один, чтобы включить другой",
		"code":  CodeContactsLimit,
		"limit": repo.MaxActiveContacts,
	})
}
