// leads.go — REST API лидов (M8, SRS §4.1): список с пагинацией и catch-up
// ?updated_since (§10.3, контракт для M9), карточка, ручная смена стадии
// через state machine M5, текущая стадия (polling fallback).
//
// Роуты вешаются на группу /api, уже обёрнутую (cmd/server) в rate limit,
// auth.Middleware и RequireRole(manager, admin) — здесь авторизация
// не перепроверяется.
package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/kanban"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// Пагинация GET /api/leads: дефолт и потолок limit.
const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// LeadsDeps — зависимости ручек /api/leads*. Machine — тот же контракт
// StageMachine, что у платёжного вебхука (объявлен в payment.go).
type LeadsDeps struct {
	Leads    repo.LeadRepo
	Msgs     repo.MessageRepo
	Payments repo.PaymentRepo
	Machine  StageMachine
	Log      *slog.Logger
}

// LeadsHandler — GET/PATCH /api/leads* (§4.1).
type LeadsHandler struct {
	deps LeadsDeps
}

func NewLeads(deps LeadsDeps) *LeadsHandler {
	return &LeadsHandler{deps: deps}
}

// Register вешает роуты на уже защищённую группу /api.
func (h *LeadsHandler) Register(api gin.IRouter) {
	api.GET("/leads", h.list)
	api.GET("/leads/:id", h.get)
	api.PATCH("/leads/:id/stage", h.patchStage)
	api.GET("/leads/:id/stage", h.getStage)
}

// --- DTO: модели БД наружу не отдаются (форма ответа — контракт для M10,
// snake_case; служебные поля ttl_task_id/pending_task клиенту не нужны) ---

type leadDTO struct {
	ID               int64      `json:"id"`
	TelegramUserID   int64      `json:"telegram_user_id"`
	Name             *string    `json:"name"`
	Phone            *string    `json:"phone"`
	TgUsername       *string    `json:"tg_username"`
	StageID          int16      `json:"stage_id"`
	MessageCount     int        `json:"message_count"`
	AntiSpamCount    int        `json:"anti_spam_count"`
	ManualResolution bool       `json:"manual_resolution"`
	EscalatedAt      *time.Time `json:"escalated_at,omitempty"`
	ConsentGivenAt   *time.Time `json:"consent_given_at,omitempty"`
	LastActivityAt   time.Time  `json:"last_activity_at"`
	CreatedAt        time.Time  `json:"created_at"`
}

func toLeadDTO(l *models.Lead) leadDTO {
	return leadDTO{
		ID:               l.ID,
		TelegramUserID:   l.TelegramUserID,
		Name:             l.Name,
		Phone:            l.Phone,
		TgUsername:       l.TgUsername,
		StageID:          l.StageID,
		MessageCount:     l.MessageCount,
		AntiSpamCount:    l.AntiSpamCount,
		ManualResolution: l.ManualResolution,
		EscalatedAt:      l.EscalatedAt,
		ConsentGivenAt:   l.ConsentGivenAt,
		LastActivityAt:   l.LastActivityAt,
		CreatedAt:        l.CreatedAt,
	}
}

type messageDTO struct {
	ID        int64     `json:"id"`
	Direction string    `json:"direction"`
	Author    *string   `json:"author"` // M12: 'bot' | 'manager:<id>'; null (старые строки/inbound) UI читает как bot/лид
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

func toMessageDTO(m *models.Message) messageDTO {
	return messageDTO{
		ID: m.ID, Direction: m.Direction, Author: m.Author,
		Content: m.Content, CreatedAt: m.CreatedAt,
	}
}

func toMessageDTOs(msgs []models.Message) []messageDTO {
	out := make([]messageDTO, 0, len(msgs))
	for i := range msgs {
		out = append(out, toMessageDTO(&msgs[i]))
	}
	return out
}

type paymentEventDTO struct {
	ID             int64               `json:"id"`
	Gateway        *string             `json:"gateway"`
	Status         *string             `json:"status"`
	AmountDue      decimal.NullDecimal `json:"amount_due"`
	AmountReceived decimal.NullDecimal `json:"amount_received"`
	NetReceived    decimal.NullDecimal `json:"net_received"`
	Currency       *string             `json:"currency"`
	ToleranceOk    *bool               `json:"tolerance_ok"`
	CreatedAt      time.Time           `json:"created_at"`
}

func toPaymentEventDTOs(evs []models.PaymentEvent) []paymentEventDTO {
	out := make([]paymentEventDTO, 0, len(evs))
	for _, e := range evs {
		out = append(out, paymentEventDTO{
			ID: e.ID, Gateway: e.Gateway, Status: e.Status,
			AmountDue: e.AmountDue, AmountReceived: e.AmountReceived,
			NetReceived: e.NetReceived, Currency: e.Currency,
			ToleranceOk: e.ToleranceOk, CreatedAt: e.CreatedAt,
		})
	}
	return out
}

// list — GET /api/leads?limit&offset&updated_since (§4.1, §10.3).
// updated_since — RFC3339 (клиент M9 возвращает ts события как получил);
// сравнение строгое «позже», фильтр ложится на idx_leads_activity.
func (h *LeadsHandler) list(c *gin.Context) {
	limit := defaultPageLimit
	if raw := c.Query("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 || v > maxPageLimit {
			apiError(c, http.StatusBadRequest,
				"limit должен быть в 1..200", codeValidation)
			return
		}
		limit = v
	}
	offset := 0
	if raw := c.Query("offset"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			apiError(c, http.StatusBadRequest, "offset должен быть >= 0", codeValidation)
			return
		}
		offset = v
	}
	var updatedSince *time.Time
	if raw := c.Query("updated_since"); raw != "" {
		ts, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			apiError(c, http.StatusBadRequest,
				"updated_since должен быть временем RFC3339", codeValidation)
			return
		}
		updatedSince = &ts
	}

	leads, total, err := h.deps.Leads.List(c.Request.Context(), repo.ListLeadsParams{
		UpdatedSince: updatedSince, Limit: limit, Offset: offset,
	})
	if err != nil {
		h.internalError(c, "list leads", err)
		return
	}
	dtos := make([]leadDTO, 0, len(leads))
	for i := range leads {
		dtos = append(dtos, toLeadDTO(&leads[i]))
	}
	c.JSON(http.StatusOK, gin.H{
		"leads": dtos, "total": total, "limit": limit, "offset": offset,
	})
}

// get — GET /api/leads/:id: карточка = лид + диалог + платежи (§4.1).
// Отдельных ручек для messages/payment_events в §4.1 нет — карточка
// единственный способ M10 показать историю.
func (h *LeadsHandler) get(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	lead, err := h.deps.Leads.GetByID(c.Request.Context(), id)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		apiError(c, http.StatusNotFound, "лид не найден", codeNotFound)
		return
	case err != nil:
		h.internalError(c, "get lead", err)
		return
	}
	msgs, err := h.deps.Msgs.ListByLead(c.Request.Context(), id, 0)
	if err != nil {
		h.internalError(c, "get lead: messages", err)
		return
	}
	payments, err := h.deps.Payments.ListByLead(c.Request.Context(), id)
	if err != nil {
		h.internalError(c, "get lead: payments", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"lead":           toLeadDTO(lead),
		"messages":       toMessageDTOs(msgs),
		"payment_events": toPaymentEventDTOs(payments),
	})
}

type patchStageRequest struct {
	StageID *int16 `json:"stage_id" binding:"required"`
}

// patchStage — PATCH /api/leads/:id/stage: ручной перевод через state
// machine M5 (ActorManager). Маппинг ошибок — контракт M5→M8:
// ErrInvalidTransition → 400, ErrStageConflict → 409 (§4.2).
func (h *LeadsHandler) patchStage(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var req patchStageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apiError(c, http.StatusBadRequest, "требуется поле stage_id", codeValidation)
		return
	}
	to := *req.StageID
	if !kanban.ValidStage(to) {
		apiError(c, http.StatusBadRequest, "stage_id вне доски 1..8", codeValidation)
		return
	}

	// Кто перевёл — в reason: он уходит в лог state machine и в событие
	// stage_change (crm:events) — доска M9/M10 видит автора.
	performedBy := "manager"
	if claims, ok := auth.ClaimsFrom(c); ok {
		performedBy = "manager:" + claims.Subject
	}
	lead, err := h.deps.Machine.Transition(c.Request.Context(), id, to,
		kanban.ActorManager, "PATCH /api/leads/:id/stage by "+performedBy)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		apiError(c, http.StatusNotFound, "лид не найден", codeNotFound)
		return
	case errors.Is(err, kanban.ErrInvalidTransition):
		apiError(c, http.StatusBadRequest,
			"переход запрещён таблицей стадий §3.1", codeInvalidTransition)
		return
	case errors.Is(err, kanban.ErrStageConflict):
		apiError(c, http.StatusConflict,
			"стадия лида изменена конкурентно, обновите доску", codeStageConflict)
		return
	case err != nil:
		h.internalError(c, "patch stage", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"lead": toLeadDTO(lead)})
}

// getStage — GET /api/leads/:id/stage: polling fallback (§4.1, §10.1 —
// Redis pub/sub down → доска опрашивает стадии раз в 5 с).
func (h *LeadsHandler) getStage(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	lead, err := h.deps.Leads.GetByID(c.Request.Context(), id)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		apiError(c, http.StatusNotFound, "лид не найден", codeNotFound)
		return
	case err != nil:
		h.internalError(c, "get stage", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"lead_id":          lead.ID,
		"stage_id":         lead.StageID,
		"last_activity_at": lead.LastActivityAt,
	})
}

func (h *LeadsHandler) internalError(c *gin.Context, what string, err error) {
	// Причина — в лог, клиенту без деталей (SQL/секреты в ответы не текут).
	h.deps.Log.Error("api: "+what, "error", err)
	apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
}
