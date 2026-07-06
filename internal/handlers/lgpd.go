// lgpd.go — LGPD-ручки (M8, SRS §9): erasure («право на забвение») и export
// (право на доступ). Обе под тем же auth-гейтом /api, роли manager/admin
// (§4.1). Обе пишут след в lgpd_audit (§9.2) и явно сообщают о сохранении
// финансовых записей (§9.3).
package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/lgpd"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// LGPDDeps — зависимости ручек /api/lgpd/*.
type LGPDDeps struct {
	LGPD     repo.LGPDRepo
	Msgs     repo.MessageRepo
	Payments repo.PaymentRepo
	// Salt — LGPD_SALT (§9.3): хеш telegram_user_id при erasure.
	Salt string
	Log  *slog.Logger
}

// LGPDHandler — DELETE .../erase и GET .../export (§4.1).
type LGPDHandler struct {
	deps LGPDDeps
}

func NewLGPD(deps LGPDDeps) *LGPDHandler {
	return &LGPDHandler{deps: deps}
}

// Register вешает роуты на уже защищённую группу /api.
func (h *LGPDHandler) Register(api gin.IRouter) {
	api.DELETE("/lgpd/leads/:id/erase", h.erase)
	api.GET("/lgpd/leads/:id/export", h.export)
}

// erase — DELETE /api/lgpd/leads/:id/erase (§9.1, AQ²-4): soft delete +
// анонимизация + '[DELETED]' в messages + хеш telegram_user_id + audit,
// всё одной транзакцией repo.LGPDRepo.Erase. payment_events не трогаются.
func (h *LGPDHandler) erase(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	// Лид нужен ДО erasure: хеш считается от живого telegram_user_id.
	lead, err := h.deps.LGPD.GetLeadAny(c.Request.Context(), id)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		apiError(c, http.StatusNotFound, "лид не найден", codeNotFound)
		return
	case err != nil:
		h.internalError(c, "erase: get lead", err)
		return
	}
	if lead.DeletedAt.Valid {
		// Повторный erase не перезатирает хеш и не плодит audit-записи.
		apiError(c, http.StatusNotFound, "лид уже стёрт", codeNotFound)
		return
	}

	hashed := lgpd.HashTelegramUserID(lead.TelegramUserID, h.deps.Salt)
	err = h.deps.LGPD.Erase(c.Request.Context(), id, hashed,
		h.performedBy(c), c.ClientIP())
	switch {
	case errors.Is(err, repo.ErrNotFound):
		// Гонка двух erase: транзакция-неудачник промахнулась мимо guard.
		apiError(c, http.StatusNotFound, "лид уже стёрт", codeNotFound)
		return
	case err != nil:
		h.internalError(c, "erase", err)
		return
	}

	h.deps.Log.Info("lgpd: erasure выполнен", "lead_id", id, "performed_by", h.performedBy(c))
	c.JSON(http.StatusOK, gin.H{
		"status":  "erased",
		"lead_id": id,
		// §9.3: явная пометка о сохранении финансовых записей — обязательна.
		"financial_records": lgpd.FinancialRecordsNote,
	})
}

type lgpdAuditDTO struct {
	ID          int64     `json:"id"`
	Action      string    `json:"action"`
	PerformedBy string    `json:"performed_by"`
	IPAddress   *string   `json:"ip_address,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// export — GET /api/lgpd/leads/:id/export (§9.1): JSON с messages,
// payment_events и lgpd_audit. Работает и для стёртого лида (право на
// доступ не гаснет после erasure; содержимое к тому моменту обезличено).
// Сам export тоже действие с персональными данными — пишется в audit (§9.2).
func (h *LGPDHandler) export(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	lead, err := h.deps.LGPD.GetLeadAny(c.Request.Context(), id)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		apiError(c, http.StatusNotFound, "лид не найден", codeNotFound)
		return
	case err != nil:
		h.internalError(c, "export: get lead", err)
		return
	}

	msgs, err := h.deps.Msgs.ListByLead(c.Request.Context(), id, 0)
	if err != nil {
		h.internalError(c, "export: messages", err)
		return
	}
	payments, err := h.deps.Payments.ListByLead(c.Request.Context(), id)
	if err != nil {
		h.internalError(c, "export: payments", err)
		return
	}

	// След ДО выдачи данных: не удалось зафиксировать доступ — данные
	// не уходят (§9.2: аудит не best-effort).
	ip := c.ClientIP()
	err = h.deps.LGPD.CreateAudit(c.Request.Context(), &models.LGPDAudit{
		Action:      lgpd.ActionExport,
		LeadID:      &id,
		PerformedBy: h.performedBy(c),
		IPAddress:   &ip,
	})
	if err != nil {
		h.internalError(c, "export: audit", err)
		return
	}
	audit, err := h.deps.LGPD.ListAuditByLead(c.Request.Context(), id)
	if err != nil {
		h.internalError(c, "export: audit list", err)
		return
	}
	auditDTOs := make([]lgpdAuditDTO, 0, len(audit))
	for _, a := range audit {
		auditDTOs = append(auditDTOs, lgpdAuditDTO{
			ID: a.ID, Action: a.Action, PerformedBy: a.PerformedBy,
			IPAddress: a.IPAddress, CreatedAt: a.CreatedAt,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"lead_id":        id,
		"erased":         lead.DeletedAt.Valid,
		"messages":       toMessageDTOs(msgs),
		"payment_events": toPaymentEventDTOs(payments),
		"lgpd_audit":     auditDTOs,
		// §9.3: явная пометка о сохранении финансовых записей — обязательна.
		"financial_records": lgpd.FinancialRecordsNote,
	})
}

// performedBy — идентификатор менеджера для lgpd_audit (§9.2) из JWT claims.
func (h *LGPDHandler) performedBy(c *gin.Context) string {
	if claims, ok := auth.ClaimsFrom(c); ok {
		return claims.Role + ":" + claims.Subject
	}
	return "unknown" // недостижимо за auth.Middleware; страховка для тестов
}

func (h *LGPDHandler) internalError(c *gin.Context, what string, err error) {
	h.deps.Log.Error("lgpd: "+what, "error", err)
	apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
}
