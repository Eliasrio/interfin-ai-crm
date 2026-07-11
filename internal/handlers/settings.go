// settings.go — M13: настройки CRM из UI (таблица 0014, сервис
// internal/settings).
//
//	GET   /api/settings — эффективные значения (manager|admin);
//	PATCH /api/settings — переопределения (ТОЛЬКО admin, RequireRole на
//	      роуте поверх общего гейта группы /api).
//
// Тело PATCH — объект {ключ: минуты}: {"takeover.reminder_minutes": 5}.
// Валидация — известный ключ и целое 1..1440, иначе 400 и НИ ОДНО значение
// из запроса не применяется (всё или ничего, без частичных записей).
package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/settings"
)

// SettingsDeps — зависимости ручек /api/settings. Svc — боевой
// settings.Service (в тестах — он же поверх фейкового репозитория:
// кэш и валидация — часть контракта).
type SettingsDeps struct {
	Svc *settings.Service
	Log *slog.Logger
}

// SettingsHandler — GET/PATCH /api/settings.
type SettingsHandler struct {
	deps SettingsDeps
}

func NewSettings(deps SettingsDeps) *SettingsHandler {
	return &SettingsHandler{deps: deps}
}

// Register вешает роуты на уже защищённую группу /api. PATCH дополнительно
// закрыт RequireRole(admin): настройки меняет только admin (task M13 §2).
func (h *SettingsHandler) Register(api gin.IRouter) {
	api.GET("/settings", h.get)
	api.PATCH("/settings", auth.RequireRole(auth.RoleAdmin), h.patch)
}

func (h *SettingsHandler) get(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"settings": h.deps.Svc.All(c.Request.Context())})
}

func (h *SettingsHandler) patch(c *gin.Context) {
	var req map[string]json.Number
	dec := json.NewDecoder(c.Request.Body)
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil || len(req) == 0 {
		apiError(c, http.StatusBadRequest,
			"требуется объект {ключ: минуты}, например {\"takeover.reminder_minutes\": 10}",
			codeValidation)
		return
	}

	// Сначала валидация ВСЕХ значений, потом запись: мусор в одном ключе
	// не оставляет вторую половину запроса применённой.
	values := make(map[string]int, len(req))
	for key, raw := range req {
		// Неизвестный ключ → ERR_UNKNOWN_KEY (EP-01). Сюда же попадает
		// служебный emma_panel.pin_hash и остальные строковые ключи панели:
		// они управляются ТОЛЬКО через /api/emma/*, наружу их не видно.
		if _, known := settings.Defaults[key]; !known {
			apiError(c, http.StatusBadRequest, "неизвестный ключ настройки: "+key, codeUnknownKey)
			return
		}
		v, err := raw.Int64()
		if err != nil || v < settings.MinMinutes || v > settings.MaxMinutes {
			apiError(c, http.StatusBadRequest,
				key+": минуты должны быть целым числом 1..1440", codeValidation)
			return
		}
		values[key] = int(v)
	}

	for key, v := range values {
		if err := h.deps.Svc.SetMinutes(c.Request.Context(), key, v); err != nil {
			h.deps.Log.Error("settings: set", "key", key, "error", err)
			apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"settings": h.deps.Svc.All(c.Request.Context())})
}
