// takeover.go — M13: PATCH /api/leads/:id/mode — кнопки «✋ Взять в работу»
// и «🤖 Вернуть Эмме» в карточке лида.
//
//	→ human: taken_by = sub из JWT, пауза автопилота снимается (менеджер
//	         главнее паузы — состояние не смешивается);
//	→ bot:   taken_by = NULL, пауза снимается (Эмма сразу активна).
//
// Идемпотентно: повтор того же режима — 200 (double click, вторая вкладка);
// повтор human другим менеджером просто переписывает taken_by — права
// «кто может брать чужого лида» вне скоупа M13 (любой manager может всё).
// Каждый успешный PATCH публикует WS-событие dialog_mode.
package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// TakeoverDeps — зависимости ручки режима диалога.
type TakeoverDeps struct {
	Leads repo.LeadRepo
	Pub   events.Publisher
	Log   *slog.Logger
}

// TakeoverHandler — PATCH /api/leads/:id/mode.
type TakeoverHandler struct {
	deps TakeoverDeps
}

func NewTakeover(deps TakeoverDeps) *TakeoverHandler {
	return &TakeoverHandler{deps: deps}
}

// Register вешает роут на уже защищённую группу /api (manager|admin).
func (h *TakeoverHandler) Register(api gin.IRouter) {
	api.PATCH("/leads/:id/mode", h.patchMode)
}

type patchModeRequest struct {
	Mode string `json:"mode"`
}

func (h *TakeoverHandler) patchMode(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var req patchModeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apiError(c, http.StatusBadRequest, "требуется поле mode", codeValidation)
		return
	}
	mode := strings.TrimSpace(req.Mode)
	if mode != models.DialogModeBot && mode != models.DialogModeHuman {
		apiError(c, http.StatusBadRequest, "mode должен быть \"bot\" или \"human\"", codeValidation)
		return
	}

	// Стёртый/несуществующий лид → 404 до записи, как остальные ручки M8.
	lead, err := h.deps.Leads.GetByID(c.Request.Context(), id)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		apiError(c, http.StatusNotFound, "лид не найден", codeNotFound)
		return
	case err != nil:
		h.deps.Log.Error("api: patch mode: get lead", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}

	fields := map[string]interface{}{
		"dialog_mode":        mode,
		"bot_silenced_until": nil, // оба перехода снимают паузу автопилота
		"taken_by":           nil,
	}
	var takenBy *int64
	if mode == models.DialogModeHuman {
		takenBy = managerID(c)
		fields["taken_by"] = takenBy
	}
	if err := h.deps.Leads.UpdateFields(c.Request.Context(), id, fields); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			apiError(c, http.StatusNotFound, "лид не найден", codeNotFound)
			return
		}
		h.deps.Log.Error("api: patch mode: update", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	lead.DialogMode = mode
	lead.BotSilencedUntil = nil
	lead.TakenBy = takenBy

	// Fire-and-forget, как все crm:events (§10.3).
	if err := h.deps.Pub.Publish(c.Request.Context(),
		events.DialogModeEvent(lead, "PATCH /api/leads/:id/mode by "+managerAuthor(c))); err != nil {
		h.deps.Log.Warn("api: событие dialog_mode не опубликовано",
			"lead_id", id, "error", err)
	}

	c.JSON(http.StatusOK, gin.H{"lead": toLeadDTO(lead)})
}

// managerID — id менеджера из JWT (sub — строка с числом, M7). Недоступные
// claims недостижимы за auth.Middleware; nil — страховка для тестов.
func managerID(c *gin.Context) *int64 {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		return nil
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		return nil
	}
	return &id
}
