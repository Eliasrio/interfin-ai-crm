// scenario.go — роуты /api/emma/scenario (EP-05, ТЗ §3 вкладка 5):
// приветствие /start, кнопка «Связаться с менеджером», текст подтверждения
// handoff. Ручка — тонкая обёртка над строковыми ключами settings EP-01
// (emma_panel.*): воркер читает те же ключи с кэшем 30 с, правка доезжает
// до Эммы без рестарта.
package emma

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/settings"
)

// ScenarioDeps — зависимости роутов scenario.
type ScenarioDeps struct {
	Settings Settings
	Log      *slog.Logger
}

// ScenarioHandler — GET/PATCH scenario.
type ScenarioHandler struct {
	deps ScenarioDeps
}

func NewScenario(deps ScenarioDeps) *ScenarioHandler {
	return &ScenarioHandler{deps: deps}
}

// Register вешает роуты на защищённую группу /api/emma (контракт EP-01).
func (h *ScenarioHandler) Register(g gin.IRouter) {
	g.GET("/scenario", h.get)
	g.PATCH("/scenario", h.patch)
}

// scenarioJSON — эффективное состояние вкладки 5 (переопределение или
// дефолт) — контракт фронта EP-07 и тело ответа GET/PATCH.
func (h *ScenarioHandler) scenarioJSON(ctx context.Context) gin.H {
	return gin.H{
		"welcome_text":           h.deps.Settings.String(ctx, settings.KeyWelcomeText),
		"manager_button_enabled": h.deps.Settings.String(ctx, settings.KeyManagerButtonEnabled) == "true",
		"manager_button_text":    h.deps.Settings.String(ctx, settings.KeyManagerButtonText),
		"handoff_confirm_text":   h.deps.Settings.String(ctx, settings.KeyHandoffConfirmText),
		"manager_mention":        h.deps.Settings.String(ctx, settings.KeyManagerMention),
	}
}

// GET /api/emma/scenario.
func (h *ScenarioHandler) get(c *gin.Context) {
	c.JSON(http.StatusOK, h.scenarioJSON(c.Request.Context()))
}

// patchScenarioReq — тело PATCH: nil-поле не меняется (частичное
// обновление, task EP-05 §4).
type patchScenarioReq struct {
	WelcomeText          *string `json:"welcome_text"`
	ManagerButtonEnabled *bool   `json:"manager_button_enabled"`
	ManagerButtonText    *string `json:"manager_button_text"`
	HandoffConfirmText   *string `json:"handoff_confirm_text"`
	ManagerMention       *string `json:"manager_mention"`
}

// mentionRe — telegram-username: 5-32 символа, латиница/цифры/подчёркивание
// (валидация manager_mention; ведущая @ срезается до проверки).
var mentionRe = regexp.MustCompile(`^[A-Za-z0-9_]{5,32}$`)

// PATCH /api/emma/scenario — частичное обновление ключей settings.
// Инвариант вкладки 5: включённая кнопка обязана иметь текст — проверяется
// РЕЗУЛЬТИРУЮЩЕЕ состояние (включение без текста и стирание текста при
// включённой кнопке — оба 400).
func (h *ScenarioHandler) patch(c *gin.Context) {
	var req patchScenarioReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiError(c, http.StatusBadRequest, "тело запроса не разобрано", codeValidation)
		return
	}
	if req.WelcomeText == nil && req.ManagerButtonEnabled == nil &&
		req.ManagerButtonText == nil && req.HandoffConfirmText == nil &&
		req.ManagerMention == nil {
		apiError(c, http.StatusBadRequest, "нужно хотя бы одно поле", codeValidation)
		return
	}
	ctx := c.Request.Context()

	enabled := h.deps.Settings.String(ctx, settings.KeyManagerButtonEnabled) == "true"
	if req.ManagerButtonEnabled != nil {
		enabled = *req.ManagerButtonEnabled
	}
	buttonText := h.deps.Settings.String(ctx, settings.KeyManagerButtonText)
	if req.ManagerButtonText != nil {
		buttonText = strings.TrimSpace(*req.ManagerButtonText)
	}
	if enabled && buttonText == "" {
		apiError(c, http.StatusBadRequest,
			"включённая кнопка менеджера требует непустого текста кнопки", codeValidation)
		return
	}

	// Запись по ключам — только предоставленные поля. Тексты триммятся:
	// «пустое» welcome из одних пробелов должно читаться воркером как пусто.
	writes := map[string]*string{}
	if req.WelcomeText != nil {
		v := strings.TrimSpace(*req.WelcomeText)
		writes[settings.KeyWelcomeText] = &v
	}
	if req.ManagerButtonEnabled != nil {
		v := "false"
		if *req.ManagerButtonEnabled {
			v = "true"
		}
		writes[settings.KeyManagerButtonEnabled] = &v
	}
	if req.ManagerButtonText != nil {
		writes[settings.KeyManagerButtonText] = &buttonText
	}
	if req.HandoffConfirmText != nil {
		v := strings.TrimSpace(*req.HandoffConfirmText)
		writes[settings.KeyHandoffConfirmText] = &v
	}
	if req.ManagerMention != nil {
		// Храним без @ (воркер добавляет сам); пусто = упоминание выключено.
		v := strings.TrimPrefix(strings.TrimSpace(*req.ManagerMention), "@")
		if v != "" && !mentionRe.MatchString(v) {
			apiError(c, http.StatusBadRequest,
				"упоминание менеджера: telegram-username из 5-32 латинских букв/цифр/подчёркиваний", codeValidation)
			return
		}
		writes[settings.KeyManagerMention] = &v
	}
	for key, val := range writes {
		if err := h.deps.Settings.SetString(ctx, key, *val); err != nil {
			h.deps.Log.Error("emma: scenario patch", "key", key, "error", err)
			apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
			return
		}
	}

	h.deps.Log.Info("emma: сценарий обновлён",
		"button_enabled", enabled, "welcome_set", req.WelcomeText != nil)
	c.JSON(http.StatusOK, h.scenarioJSON(ctx))
}
