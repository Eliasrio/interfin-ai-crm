// prompt.go — роуты /api/emma/prompt* (EP-02, ТЗ §3/§6): активный промпт,
// история версий, restore. Все ручки висят на защищённой группе (JWT +
// RequireRole(admin) + RequirePIN + Audit) — про PIN здесь ничего не знают.
//
// Версии не мутируются: PUT и restore создают новую активную строку через
// EmmaPromptRepo.CreateVersion, прежняя активная остаётся в истории сама.
package emma

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
	"github.com/interfin/interfin-ai-crm/internal/settings"
	"github.com/interfin/interfin-ai-crm/internal/worker"
)

// Коды ошибок контура промпта — контракт для фронта EP-07.
const (
	// CodePromptTooLong — 400: оценка токенов текста превышает лимит
	// emma_panel.prompt_token_limit; тело дополнено estimate и limit.
	CodePromptTooLong = "PROMPT_TOO_LONG"
	codeNotFound      = "ERR_NOT_FOUND" // 404 (конвенция handlers §4.2)
)

// historyPerPage — размер страницы истории версий (task EP-02 §6).
const historyPerPage = 50

// promptStyles — допустимые значения style (CHECK в БД, 0016).
var promptStyles = map[string]bool{
	models.EmmaStyleFormal:   true,
	models.EmmaStyleFriendly: true,
	models.EmmaStyleNeutral:  true,
	models.EmmaStyleExpert:   true,
}

// PromptDeps — зависимости роутов prompt*.
type PromptDeps struct {
	Prompts  repo.EmmaPromptRepo
	Settings Settings // лимит токенов emma_panel.prompt_token_limit
	Log      *slog.Logger
}

// PromptHandler — GET/PUT prompt, GET history, GET history/:vid,
// POST history/:vid/restore.
type PromptHandler struct {
	deps PromptDeps
}

func NewPrompt(deps PromptDeps) *PromptHandler {
	return &PromptHandler{deps: deps}
}

// Register вешает роуты на защищённую группу /api/emma (контракт EP-01).
func (h *PromptHandler) Register(g gin.IRouter) {
	g.GET("/prompt", h.get)
	g.PUT("/prompt", h.put)
	g.GET("/prompt/history", h.history)
	g.GET("/prompt/history/:vid", h.version)
	g.POST("/prompt/history/:vid/restore", h.restore)
}

// tokenLimit — лимит редактора из settings (дефолт 1200, меняется только
// с сервера — ТЗ §3). Нечисловое/неположительное значение в БД приравнивается
// к отсутствию строки — дефолт (дисциплина settings.Minutes).
func (h *PromptHandler) tokenLimit(ctx context.Context) int {
	limit, err := strconv.Atoi(h.deps.Settings.String(ctx, settings.KeyPromptTokenLimit))
	if err != nil || limit <= 0 {
		limit, _ = strconv.Atoi(settings.StringDefaults[settings.KeyPromptTokenLimit])
	}
	return limit
}

// promptResponse — тело GET /prompt (оно же — ответ PUT и restore, task §6).
// token_estimate — та же метрика len/4, что у Budgeter (worker.EstimateTokens):
// счётчик фронта EP-07 обязан совпадать с воркером.
func (h *PromptHandler) promptResponse(ctx context.Context, v *models.EmmaPromptVersion) gin.H {
	return gin.H{
		"version_id":       v.ID,
		"system_prompt":    v.SystemPrompt,
		"forbidden_topics": topicsOf(v, h.deps.Log),
		"style":            v.Style,
		"created_at":       v.CreatedAt,
		"token_estimate":   worker.EstimateTokens(v.SystemPrompt),
		"token_limit":      h.tokenLimit(ctx),
	}
}

// GET /api/emma/prompt — активная версия.
func (h *PromptHandler) get(c *gin.Context) {
	ctx := c.Request.Context()
	v, err := h.deps.Prompts.GetCurrent(ctx)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		// Достижимо только на БД без сида 0021: Эмма при этом работает на
		// константе-fallback, но редактировать из панели нечего.
		apiError(c, http.StatusNotFound,
			"активной версии промпта нет (миграция 0021 не накатана)", codeNotFound)
		return
	case err != nil:
		h.deps.Log.Error("emma: prompt get", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	c.JSON(http.StatusOK, h.promptResponse(ctx, v))
}

type promptBody struct {
	SystemPrompt    string   `json:"system_prompt"`
	ForbiddenTopics []string `json:"forbidden_topics"`
	Style           string   `json:"style"`
}

// PUT /api/emma/prompt {system_prompt, forbidden_topics, style} — новая
// активная версия (прежняя остаётся в истории).
func (h *PromptHandler) put(c *gin.Context) {
	sub, ok := managerID(c)
	if !ok {
		return
	}
	var req promptBody
	if err := c.ShouldBindJSON(&req); err != nil {
		apiError(c, http.StatusBadRequest, "тело запроса не разобрано", codeValidation)
		return
	}
	if strings.TrimSpace(req.SystemPrompt) == "" {
		apiError(c, http.StatusBadRequest, "system_prompt пуст", codeValidation)
		return
	}
	if !promptStyles[req.Style] {
		apiError(c, http.StatusBadRequest,
			"style — один из formal/friendly/neutral/expert", codeValidation)
		return
	}
	topics := make([]string, 0, len(req.ForbiddenTopics))
	for _, t := range req.ForbiddenTopics {
		if t = strings.TrimSpace(t); t != "" {
			topics = append(topics, t)
		}
	}
	ctx := c.Request.Context()

	// Лимит — на ТЕКСТ промпта (task §6): секции тем/стиля короткие, а
	// контакты/файлы/RAG считаются на общий бюджет 5000 отдельно.
	limit := h.tokenLimit(ctx)
	if estimate := worker.EstimateTokens(req.SystemPrompt); estimate > limit {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error":    "промпт длиннее лимита токенов",
			"code":     CodePromptTooLong,
			"estimate": estimate,
			"limit":    limit,
		})
		return
	}

	v, ok := h.createVersion(c, &promptFields{
		Text: req.SystemPrompt, Topics: topics, Style: req.Style,
	}, sub)
	if !ok {
		return
	}
	h.deps.Log.Info("emma: prompt обновлён", "manager_id", sub, "version_id", v.ID)
	c.JSON(http.StatusOK, h.promptResponse(ctx, v))
}

// GET /api/emma/prompt/history?page= — страница истории (новые → старые).
func (h *PromptHandler) history(c *gin.Context) {
	page := 1
	if raw := c.Query("page"); raw != "" {
		p, err := strconv.Atoi(raw)
		if err != nil || p < 1 {
			apiError(c, http.StatusBadRequest, "page — целое число от 1", codeValidation)
			return
		}
		page = p
	}
	items, total, err := h.deps.Prompts.History(c.Request.Context(), page, historyPerPage)
	if err != nil {
		h.deps.Log.Error("emma: prompt history", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	out := make([]gin.H, 0, len(items))
	for _, it := range items {
		out = append(out, gin.H{
			"id":         it.ID,
			"created_at": it.CreatedAt,
			"created_by": it.CreatedBy, // null — сид миграции / удалённый менеджер
			"preview":    it.Preview,
			"style":      it.Style,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": out, "total": total, "page": page})
}

// GET /api/emma/prompt/history/:vid — полная версия для просмотра.
func (h *PromptHandler) version(c *gin.Context) {
	v, ok := h.versionByParam(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"version_id":       v.ID,
		"system_prompt":    v.SystemPrompt,
		"forbidden_topics": topicsOf(v, h.deps.Log),
		"style":            v.Style,
		"created_at":       v.CreatedAt,
		"created_by":       v.CreatedBy,
		"is_current":       v.IsCurrent,
	})
}

// POST /api/emma/prompt/history/:vid/restore — новая активная версия — копия
// vid (все три поля: текст, темы, стиль — ТЗ §3, это пишется в UI
// подтверждения). Прежняя активная остаётся в истории автоматически.
func (h *PromptHandler) restore(c *gin.Context) {
	sub, ok := managerID(c)
	if !ok {
		return
	}
	src, ok := h.versionByParam(c)
	if !ok {
		return
	}
	topics := make(models.JSONB, len(src.ForbiddenTopics))
	copy(topics, src.ForbiddenTopics)
	v, ok := h.createVersion(c, &promptFields{
		Text: src.SystemPrompt, RawTopics: topics, Style: src.Style,
	}, sub)
	if !ok {
		return
	}
	h.deps.Log.Info("emma: prompt восстановлен из версии",
		"manager_id", sub, "source_version_id", src.ID, "version_id", v.ID)
	c.JSON(http.StatusOK, h.promptResponse(c.Request.Context(), v))
}

// --- внутренности ---

// promptFields — поля новой версии; RawTopics (готовый JSONB restore)
// имеет приоритет над Topics (список из PUT).
type promptFields struct {
	Text      string
	Topics    []string
	RawTopics models.JSONB
	Style     string
}

// createVersion — общее ядро PUT/restore: created_by из sub, JSONB тем,
// вставка через репозиторий. false — ответ уже отправлен (500).
func (h *PromptHandler) createVersion(c *gin.Context, f *promptFields, sub string) (*models.EmmaPromptVersion, bool) {
	raw := f.RawTopics
	if raw == nil {
		var err error
		if raw, err = json.Marshal(f.Topics); err != nil {
			h.deps.Log.Error("emma: prompt: marshal topics", "error", err)
			apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
			return nil, false
		}
	}
	v := &models.EmmaPromptVersion{
		SystemPrompt:    f.Text,
		ForbiddenTopics: models.JSONB(raw),
		Style:           f.Style,
		CreatedBy:       createdBy(sub, h.deps.Log),
	}
	if err := h.deps.Prompts.CreateVersion(c.Request.Context(), v); err != nil {
		// Сюда попадает и проигравший гонку конкурентных PUT (уникальный
		// индекс 0016): вторая активная невозможна, владелец повторит запрос.
		h.deps.Log.Error("emma: prompt: create version", "manager_id", sub, "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return nil, false
	}
	return v, true
}

// versionByParam — версия по :vid; нечисловой и несуществующий id наружу
// не различаются — 404 (task §6). false — ответ уже отправлен.
func (h *PromptHandler) versionByParam(c *gin.Context) (*models.EmmaPromptVersion, bool) {
	vid, err := strconv.ParseInt(c.Param("vid"), 10, 64)
	if err != nil || vid < 1 {
		apiError(c, http.StatusNotFound, "версия не найдена", codeNotFound)
		return nil, false
	}
	v, err := h.deps.Prompts.GetByID(c.Request.Context(), vid)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		apiError(c, http.StatusNotFound, "версия не найдена", codeNotFound)
		return nil, false
	case err != nil:
		h.deps.Log.Error("emma: prompt version get", "vid", vid, "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return nil, false
	}
	return v, true
}

// createdBy — sub JWT (id менеджера строкой, контракт M7) → *int64 колонки.
// Нечисловой sub недостижим с боевым Issuer — след в логе вместо паники.
func createdBy(sub string, log *slog.Logger) *int64 {
	id, err := strconv.ParseInt(sub, 10, 64)
	if err != nil {
		log.Warn("emma: prompt: sub не число, created_by останется NULL", "sub", sub)
		return nil
	}
	return &id
}

// topicsOf — JSONB → []string для ответа (битый JSONB → пустой список).
func topicsOf(v *models.EmmaPromptVersion, log *slog.Logger) []string {
	topics := []string{}
	if len(v.ForbiddenTopics) > 0 {
		if err := json.Unmarshal(v.ForbiddenTopics, &topics); err != nil {
			log.Warn("emma: forbidden_topics версии не разобраны",
				"version_id", v.ID, "error", err)
			topics = []string{}
		}
	}
	return topics
}
