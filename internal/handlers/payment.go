// payment.go — M6: приём коллбэков крипто-шлюза CryptoBot (SRS §3.3, §5.5).
//
// Цепочка POST /webhook/payment:
//
//	HMAC-подпись (403) → разбор → replay-защита: timestamp ±окно и
//	nonce=update_id (403) → запись payment_events (§8.3) → tolerance §3.3 →
//	переход стадии через state machine M5 (ActorPayment) → payment_received.
//
// Порядок «сначала payment_events, потом переход» сознательный: запись о
// деньгах — фискальный источник правды (CLAUDE.md §4.8), её не теряем, даже
// если переход упадёт; дубль строки при ретрае отсекает уникальный индекс
// 0008 (Create идемпотентен). Провал после захвата nonce возвращает nonce
// (Release): шлюз ретраит до 2xx, и ретрай не должен читаться как replay.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/kanban"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/payment"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// StageMachine — контракт M5 для платёжного вебхука (kanban.Machine подходит
// структурно; в тестах — фейк).
type StageMachine interface {
	Transition(ctx context.Context, leadID int64, to int16, actor kanban.Actor, reason string) (*models.Lead, error)
}

// transitionAttempts — ретраи ErrStageConflict: CAS промахнулся из-за
// конкурирующего перехода. Payment переопределяет авто-триггеры (§3.1),
// поэтому пробуем ещё раз от свежей стадии, а не сдаёмся.
const transitionAttempts = 3

// PaymentWebhook — зависимости цепочки POST /webhook/payment.
type PaymentWebhook struct {
	leads     repo.LeadRepo
	payments  repo.PaymentRepo
	machine   StageMachine
	nonces    payment.NonceStore
	pub       events.Publisher
	token     string          // активный токен CryptoBot — материал HMAC-ключа (§5.5)
	window    time.Duration   // replay-окно ±5 мин (§5.5)
	tolerance decimal.Decimal // kanban.underpaid_tolerance_pct — из конфига, не хардкод (§3.3)
	log       *slog.Logger
}

// PaymentWebhookDeps — конструкторный набор (зависимостей много, позиционные
// аргументы нечитаемы).
type PaymentWebhookDeps struct {
	Leads        repo.LeadRepo
	Payments     repo.PaymentRepo
	Machine      StageMachine
	Nonces       payment.NonceStore
	Pub          events.Publisher
	Cfg          config.PaymentConfig
	TolerancePct float64 // cfg.Kanban.UnderpaidTolerancePct
	Log          *slog.Logger
}

func NewPaymentWebhook(d PaymentWebhookDeps) *PaymentWebhook {
	return &PaymentWebhook{
		leads:     d.Leads,
		payments:  d.Payments,
		machine:   d.Machine,
		nonces:    d.Nonces,
		pub:       d.Pub,
		token:     d.Cfg.ActiveToken(),
		window:    time.Duration(d.Cfg.ReplayWindowMinutes) * time.Minute,
		tolerance: decimal.NewFromFloat(d.TolerancePct),
		log:       d.Log,
	}
}

// Register вешает цепочку на POST /webhook/payment (§4.1).
func (h *PaymentWebhook) Register(r gin.IRouter) {
	r.POST("/webhook/payment", h.Handle)
}

// Handle — транспортный слой: подпись, replay-защита, коды ответов.
// Бизнес-часть (деньги, tolerance, стадия) — в process.
func (h *PaymentWebhook) Handle(c *gin.Context) {
	ctx := c.Request.Context()

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes))
	if err != nil {
		h.log.Error("payment webhook: чтение тела", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "cannot read request body", "code": "ERR_PAYMENT_READ"})
		return
	}

	// §5.5: подпись считается от сырых байт; невалидная → 403, тело не разбираем.
	if !payment.VerifySignature(h.token, body, c.GetHeader(payment.SignatureHeader)) {
		c.JSON(http.StatusForbidden, gin.H{
			"error": "invalid webhook signature", "code": "ERR_PAYMENT_FORBIDDEN"})
		return
	}

	upd, err := payment.ParseUpdate(body)
	if err != nil {
		// Подпись сошлась — это шлюз, но тело нечитаемо. Ретраи не помогут —
		// не зацикливаем их (как garbage-body у Telegram, M2); платёж виден
		// в кабинете шлюза, ошибка — в логе.
		h.log.Error("payment webhook: нечитаемое тело", "error", err)
		c.JSON(http.StatusOK, gin.H{"status": "ignored"})
		return
	}
	if upd.UpdateType != payment.UpdateInvoicePaid {
		h.log.Info("payment webhook: не invoice_paid, пропуск", "update_type", upd.UpdateType)
		c.JSON(http.StatusOK, gin.H{"status": "ignored"})
		return
	}

	// §5.5 replay-защита, слой 1: timestamp вне окна ±window → 403.
	if age := time.Since(upd.RequestDate); age > h.window || age < -h.window {
		h.log.Warn("payment webhook: request_date вне replay-окна",
			"request_date", upd.RequestDate, "window", h.window)
		c.JSON(http.StatusForbidden, gin.H{
			"error": "request timestamp outside replay window", "code": "ERR_PAYMENT_REPLAY"})
		return
	}

	inv := upd.Payload
	leadID, err := payment.LeadIDFromPayload(inv.Payload)
	if err != nil {
		// Инвойс без lead_id: деньги пришли, а связать не с кем. Ретраи не
		// исправят — 200; сигнал остаётся в логе (алерт M11).
		h.log.Error("payment webhook: инвойс без lead_id",
			"invoice_id", inv.InvoiceID, "payload", inv.Payload, "error", err)
		c.JSON(http.StatusOK, gin.H{"status": "ignored"})
		return
	}
	if _, err := h.leads.GetByID(ctx, leadID); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			h.log.Error("payment webhook: лид из payload не найден",
				"lead_id", leadID, "invoice_id", inv.InvoiceID)
			c.JSON(http.StatusOK, gin.H{"status": "ignored"})
			return
		}
		h.log.Error("payment webhook: чтение лида", "lead_id", leadID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "lead lookup failed", "code": "ERR_PAYMENT_PERSIST"})
		return
	}

	// §5.5 replay-защита, слой 2: nonce (update_id) одноразовый → повтор 403.
	nonce := strconv.FormatInt(upd.UpdateID, 10)
	fresh, err := h.nonces.Claim(ctx, nonce, 2*h.window)
	if err != nil {
		h.log.Error("payment webhook: nonce store недоступен", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "replay protection unavailable", "code": "ERR_PAYMENT_NONCE"})
		return
	}
	if !fresh {
		h.log.Warn("payment webhook: повтор nonce, отклонён", "update_id", upd.UpdateID)
		c.JSON(http.StatusForbidden, gin.H{
			"error": "duplicate update (replay)", "code": "ERR_PAYMENT_REPLAY"})
		return
	}

	if err := h.process(ctx, leadID, upd, body); err != nil {
		// Nonce возвращаем: ретрай шлюза (доставка повторяется до 2xx) не
		// должен быть отвергнут как replay. Идемпотентность добора — индекс
		// 0008 + ветка «уже в to» машины стадий.
		if rerr := h.nonces.Release(ctx, nonce); rerr != nil {
			h.log.Error("payment webhook: nonce не возвращён",
				"update_id", upd.UpdateID, "error", rerr)
		}
		h.log.Error("payment webhook: обработка платежа",
			"lead_id", leadID, "invoice_id", inv.InvoiceID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "payment processing failed", "code": "ERR_PAYMENT_PERSIST"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// process — бизнес-часть: payment_events (§8.3) → tolerance §3.3 → стадия →
// событие payment_received.
func (h *PaymentWebhook) process(ctx context.Context, leadID int64, upd *payment.Update, raw []byte) error {
	inv := upd.Payload
	due := inv.Amount
	received := inv.AmountReceived()
	net := inv.NetReceived() // gas/service fee уже вычтена — оперируем net (§3.3)

	// §3.3: net_received >= amount_due × (1 − pct/100); pct из конфига.
	minOK := due.Mul(decimal.NewFromInt(100).Sub(h.tolerance)).
		Div(decimal.NewFromInt(100))
	toleranceOK := net.GreaterThanOrEqual(minOK)

	gateway, status, currency := payment.Gateway, inv.Status, inv.Currency()
	ev := &models.PaymentEvent{
		LeadID:         leadID,
		Gateway:        &gateway,
		Status:         &status,
		AmountDue:      decimal.NullDecimal{Decimal: due, Valid: true},
		AmountReceived: decimal.NullDecimal{Decimal: received, Valid: true},
		NetReceived:    decimal.NullDecimal{Decimal: net, Valid: true},
		Currency:       &currency,
		ToleranceOk:    &toleranceOK,
		RawPayload:     models.JSONB(raw),
	}
	if err := h.payments.Create(ctx, ev); err != nil {
		return fmt.Errorf("payment_events insert: %w", err)
	}

	// §3.3: флаг ручного разбора следует за исходом платежа — недоплата
	// ставит его, успешная (в т.ч. повторная после недоплаты) оплата снимает:
	// оплаченный лид не должен висеть в списке ручного разбора.
	if err := h.leads.UpdateFields(ctx, leadID,
		map[string]interface{}{"manual_resolution": !toleranceOK}); err != nil {
		return fmt.Errorf("manual_resolution: %w", err)
	}

	target := kanban.StagePaid
	reason := fmt.Sprintf("cryptobot invoice %d: net %s из %s %s в пределах tolerance %s%%",
		inv.InvoiceID, net, due, currency, h.tolerance)
	if !toleranceOK {
		// Недоплата сверх tolerance → Stage 4 (TTL 48ч взводит машина §3.4).
		target = kanban.StageUnpaid
		reason = fmt.Sprintf("cryptobot invoice %d: недоплата — net %s из %s %s (tolerance %s%%)",
			inv.InvoiceID, net, due, currency, h.tolerance)
	}

	lead, err := h.transition(ctx, leadID, target, reason)
	if err != nil {
		return err
	}

	// Задача M6 №6: WS-событие payment_received (потребитель M9).
	// Fire-and-forget, как stage_change в машине: пропуск добирает
	// catch-up §10.3, бизнес-операцию не роняем.
	if err := h.pub.Publish(ctx, events.Event{
		Type:        events.TypePaymentReceived,
		LeadID:      leadID,
		StageID:     lead.StageID,
		Actor:       string(kanban.ActorPayment),
		Amount:      net.String(),
		Currency:    currency,
		ToleranceOk: &toleranceOK,
		Reason:      reason,
	}); err != nil {
		h.log.Warn("payment webhook: payment_received не опубликовано",
			"lead_id", leadID, "error", err)
	}

	h.log.Info("payment webhook: платёж обработан",
		"lead_id", leadID, "invoice_id", inv.InvoiceID, "net", net.String(),
		"currency", currency, "tolerance_ok", toleranceOK, "stage_id", lead.StageID)
	return nil
}

// transition — переход с ретраем ErrStageConflict: Payment переопределяет
// авто-триггеры (§3.1), конфликт CAS значит лишь «стадия сменилась под рукой» —
// повторный вызов перечитает лида и применит переход от неё.
func (h *PaymentWebhook) transition(ctx context.Context, leadID int64, to int16, reason string) (*models.Lead, error) {
	var lastErr error
	for attempt := 0; attempt < transitionAttempts; attempt++ {
		lead, err := h.machine.Transition(ctx, leadID, to, kanban.ActorPayment, reason)
		if err == nil {
			return lead, nil
		}
		if !errors.Is(err, kanban.ErrStageConflict) {
			return nil, fmt.Errorf("переход в стадию %d: %w", to, err)
		}
		lastErr = err
	}
	return nil, fmt.Errorf("переход в стадию %d не применён за %d попыток: %w",
		to, transitionAttempts, lastErr)
}
