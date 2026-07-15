// stages.go — названия этапов Kanban, редактируемые из UI (2026-07-15).
//
//	GET   /api/stages — эффективные названия (manager|admin);
//	PATCH /api/stages — переименование (ТОЛЬКО admin), тело {"1": "Новые"}.
//
// Меняются только надписи досок/карточек: id этапов и переходы state
// machine (internal/kanban) от названий не зависят. Валидация — id 1..8,
// непустое название до 60 символов; всё или ничего, как /api/settings.
package handlers

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/settings"
)

// maxStageNameChars — предел названия этапа (символы, не байты: кириллица).
const maxStageNameChars = 60

// StagesDeps — зависимости ручек /api/stages (боевой settings.Service:
// кэш 30 с и запись — часть контракта, как у /api/settings).
type StagesDeps struct {
	Svc *settings.Service
	Log *slog.Logger
}

// StagesHandler — GET/PATCH /api/stages.
type StagesHandler struct {
	deps StagesDeps
}

func NewStages(deps StagesDeps) *StagesHandler {
	return &StagesHandler{deps: deps}
}

// Register вешает роуты на уже защищённую группу /api; PATCH дополнительно
// закрыт RequireRole(admin) — паттерн /api/settings.
func (h *StagesHandler) Register(api gin.IRouter) {
	api.GET("/stages", h.get)
	api.PATCH("/stages", auth.RequireRole(auth.RoleAdmin), h.patch)
}

// names — эффективные названия всех этапов: переопределение или дефолт.
func (h *StagesHandler) names(c *gin.Context) map[string]string {
	out := make(map[string]string, settings.LastStageID)
	for id := settings.FirstStageID; id <= settings.LastStageID; id++ {
		out[strconv.Itoa(id)] = h.deps.Svc.String(c.Request.Context(), settings.StageNameKey(id))
	}
	return out
}

func (h *StagesHandler) get(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"names": h.names(c)})
}

func (h *StagesHandler) patch(c *gin.Context) {
	var req map[string]string
	if err := c.ShouldBindJSON(&req); err != nil || len(req) == 0 {
		apiError(c, http.StatusBadRequest,
			"требуется объект {этап: название}, например {\"1\": \"Новые лиды\"}", codeValidation)
		return
	}

	// Сначала валидация ВСЕХ значений, потом запись (всё или ничего).
	values := make(map[string]string, len(req))
	for rawID, name := range req {
		id, err := strconv.Atoi(rawID)
		if err != nil || id < settings.FirstStageID || id > settings.LastStageID {
			apiError(c, http.StatusBadRequest, "неизвестный этап: "+rawID, codeUnknownKey)
			return
		}
		name = strings.TrimSpace(name)
		if name == "" || utf8.RuneCountInString(name) > maxStageNameChars {
			apiError(c, http.StatusBadRequest,
				"этап "+rawID+": название — непустая строка до 60 символов", codeValidation)
			return
		}
		values[settings.StageNameKey(id)] = name
	}

	for key, name := range values {
		if err := h.deps.Svc.SetString(c.Request.Context(), key, name); err != nil {
			h.deps.Log.Error("stages: set", "key", key, "error", err)
			apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
			return
		}
	}
	h.deps.Log.Info("stages: названия обновлены", "count", len(values))
	c.JSON(http.StatusOK, gin.H{"names": h.names(c)})
}
