// language.go — M14: PATCH /api/leads/:id/language — ручная смена языка
// клиента (выпадающий список в карточке лида), если детектор ошибся.
//
// Идемпотентно: повтор того же языка — 200 (double click, вторая вкладка).
// Мусор («pt», «», 123) — 400; стёртый/несуществующий лид — 404.
// Каждый успешный PATCH публикует WS-событие lead_language (fire-and-forget).
// Роли: группа /api уже под auth.RequireRole(manager, admin) — здесь
// авторизация не перепроверяется (как режим M13).
package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/lang"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// LanguageDeps — зависимости ручки языка лида.
type LanguageDeps struct {
	Leads repo.LeadRepo
	Pub   events.Publisher
	Log   *slog.Logger
}

// LanguageHandler — PATCH /api/leads/:id/language.
type LanguageHandler struct {
	deps LanguageDeps
}

func NewLanguage(deps LanguageDeps) *LanguageHandler {
	return &LanguageHandler{deps: deps}
}

// Register вешает роут на уже защищённую группу /api (manager|admin).
func (h *LanguageHandler) Register(api gin.IRouter) {
	api.PATCH("/leads/:id/language", h.patchLanguage)
}

type patchLanguageRequest struct {
	Language string `json:"language"`
}

func (h *LanguageHandler) patchLanguage(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var req patchLanguageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apiError(c, http.StatusBadRequest, "требуется поле language", codeValidation)
		return
	}
	code := strings.TrimSpace(req.Language)
	if !lang.Valid(code) {
		apiError(c, http.StatusBadRequest,
			"language должен быть \"ru\", \"en\" или \"es\"", codeValidation)
		return
	}

	// Стёртый/несуществующий лид → 404 до записи, как остальные ручки M8.
	lead, err := h.deps.Leads.GetByID(c.Request.Context(), id)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		apiError(c, http.StatusNotFound, "лид не найден", codeNotFound)
		return
	case err != nil:
		h.deps.Log.Error("api: patch language: get lead", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}

	if err := h.deps.Leads.UpdateFields(c.Request.Context(), id,
		map[string]interface{}{"language": code}); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			apiError(c, http.StatusNotFound, "лид не найден", codeNotFound)
			return
		}
		h.deps.Log.Error("api: patch language: update", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	lead.Language = &code

	// Fire-and-forget, как все crm:events (§10.3).
	if err := h.deps.Pub.Publish(c.Request.Context(),
		events.LeadLanguageEvent(lead, "PATCH /api/leads/:id/language by "+managerAuthor(c))); err != nil {
		h.deps.Log.Warn("api: событие lead_language не опубликовано",
			"lead_id", id, "error", err)
	}

	c.JSON(http.StatusOK, gin.H{"lead": toLeadDTO(lead)})
}
