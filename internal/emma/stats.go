// stats.go — роуты /api/emma/stats и /api/emma/stats/errors (EP-06,
// ТЗ §3 вкладка 6): сводные метрики за период (ответы, скорость, токены,
// примерная стоимость, handoff, файлы, ошибки, диалоги) и журнал ошибок
// с фильтром и пагинацией. Данные — repo.EmmaStatsRepo; ручка только
// маппит периоды и собирает JSON-контракт фронта EP-07.
package emma

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// Прайс Anthropic для claude-sonnet-5, USD за 1M токенов, input и output
// отдельно (ТЗ §3: константы в коде с версией модели). Снято 2026-07-12
// со справочника цен Anthropic: стандартная цена $3/$15; до 2026-08-31
// действует вводная $2/$10 — берём стандартную, чтобы оценка не занижала
// расходы после сентября. Смена модели в конфиге требует правки констант.
const (
	pricingModel        = "claude-sonnet-5"
	priceUSDPer1MInput  = 3.0
	priceUSDPer1MOutput = 15.0
)

// pricingNote — обязательная пометка к стоимости (task §3): фронт EP-07
// показывает её рядом с суммой как есть.
const pricingNote = "примерная оценка; точный счёт — в консоли Anthropic"

// costUSD — примерная стоимость: токены × константы за 1M.
func costUSD(tokensIn, tokensOut int64) float64 {
	return float64(tokensIn)/1e6*priceUSDPer1MInput +
		float64(tokensOut)/1e6*priceUSDPer1MOutput
}

// StatsDeps — зависимости роутов статистики.
type StatsDeps struct {
	Stats repo.EmmaStatsRepo
	Log   *slog.Logger
}

// StatsHandler — GET stats, GET stats/errors.
type StatsHandler struct {
	deps StatsDeps
	// now подменяется в тестах (детерминированные границы периодов).
	now func() time.Time
}

func NewStats(deps StatsDeps) *StatsHandler {
	return &StatsHandler{deps: deps, now: time.Now}
}

// Register вешает роуты на защищённую группу /api/emma (контракт EP-01).
func (h *StatsHandler) Register(g gin.IRouter) {
	g.GET("/stats", h.get)
	g.GET("/stats/errors", h.errors)
}

// periodBounds — границы периода [from, to): скользящие окна от «сейчас»
// (day = 24 ч, week = 7 д, month = 30 д), all — с нулевого времени
// (reply-события начались с деплоя EP-06 — «за всё время» честно считает
// с этого момента, task «вне скоупа»).
func periodBounds(period string, now time.Time) (from, to time.Time, ok bool) {
	to = now
	switch period {
	case "day":
		return now.Add(-24 * time.Hour), to, true
	case "week":
		return now.AddDate(0, 0, -7), to, true
	case "month":
		return now.AddDate(0, 0, -30), to, true
	case "all":
		return time.Time{}, to, true
	}
	return time.Time{}, time.Time{}, false
}

// GET /api/emma/stats?period=day|week|month|all — сводные метрики вкладки 6.
// Контракт ответа зафиксирован contract-тестом (фронт EP-07 пишет по нему).
func (h *StatsHandler) get(c *gin.Context) {
	period := c.DefaultQuery("period", "day")
	now := h.now().UTC()
	from, to, ok := periodBounds(period, now)
	if !ok {
		apiError(c, http.StatusBadRequest,
			"period должен быть day, week, month или all", codeValidation)
		return
	}
	ctx := c.Request.Context()

	ev, err := h.deps.Stats.Stats(ctx, from, to)
	if err != nil {
		h.deps.Log.Error("emma: stats", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	// Активные диалоги — ВСЕГДА последние 24 ч, независимо от period (ТЗ §3).
	dlg, err := h.deps.Stats.DialogStats(ctx, from, to, now.Add(-24*time.Hour))
	if err != nil {
		h.deps.Log.Error("emma: dialog stats", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}

	files := make([]gin.H, 0, len(ev.Files))
	for _, f := range ev.Files {
		files = append(files, gin.H{
			"send_file_id": f.SendFileID, // null — файл удалён из библиотеки
			"name":         f.Name,
			"count":        f.Count,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"period": period,
		"from":   from.UTC(),
		"to":     to.UTC(),

		// Диалоги (leads/messages).
		"new_leads":          dlg.NewLeads,
		"active_dialogs_24h": dlg.ActiveDialogs,
		"messages_in":        dlg.MessagesIn,
		"messages_out":       dlg.MessagesOut,

		// Ответы Эммы (emma_events, reply).
		"replies":         ev.Replies,
		"avg_response_ms": ev.AvgResponseMs,
		"p95_response_ms": ev.P95ResponseMs,

		// Расходы Claude (единственный источник — reply-события).
		"tokens_in":         ev.TokensIn,
		"tokens_out":        ev.TokensOut,
		"cost_usd_estimate": costUSD(ev.TokensIn, ev.TokensOut),
		"pricing_model":     pricingModel,
		"pricing_note":      pricingNote,

		// Handoff и файлы.
		"handoffs":         ev.Handoffs,
		"files_sent_total": ev.FilesSent,
		"files_sent":       files,

		// Ошибки по видам (llm_api/telegram_api/timeout/file_not_found/kb_index).
		"errors": ev.ErrorsByKind,
	})
}

// GET /api/emma/stats/errors?type=&page=&period= — журнал ошибок: 50 на
// страницу, новые сверху, фильтр по error_kind и периоду (ТЗ §3).
func (h *StatsHandler) errors(c *gin.Context) {
	kind := c.Query("type")
	switch kind {
	case "", models.EmmaErrLLMAPI, models.EmmaErrTelegramAPI, models.EmmaErrTimeout,
		models.EmmaErrFileNotFound, models.EmmaErrKBIndex:
	default:
		apiError(c, http.StatusBadRequest,
			"type должен быть llm_api, telegram_api, timeout, file_not_found или kb_index",
			codeValidation)
		return
	}

	page := 1
	if raw := c.Query("page"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			apiError(c, http.StatusBadRequest, "page должен быть числом ≥ 1", codeValidation)
			return
		}
		page = v
	}

	from, to, ok := periodBounds(c.DefaultQuery("period", "all"), h.now().UTC())
	if !ok {
		apiError(c, http.StatusBadRequest,
			"period должен быть day, week, month или all", codeValidation)
		return
	}

	events, total, err := h.deps.Stats.ListErrors(c.Request.Context(), kind, from, to, page)
	if err != nil {
		h.deps.Log.Error("emma: stats errors", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}

	items := make([]gin.H, 0, len(events))
	for _, ev := range events {
		items = append(items, gin.H{
			"id":         ev.ID,
			"created_at": ev.CreatedAt,
			"error_kind": ev.ErrorKind,
			"detail":     ev.Detail,
			"lead_id":    ev.LeadID, // null — лид стёрт (LGPD, SET NULL)
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"errors": items,
		"total":  total,
		"page":   page,
		"limit":  50,
	})
}
