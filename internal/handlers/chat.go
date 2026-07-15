// chat.go — M12: чат менеджера и счёт из карточки лида.
//
// Три ручки на защищённой группе /api (rate limit → JWT → роли, как M8):
//
//	GET  /api/leads/:id/messages — история переписки (limit/before_id);
//	POST /api/leads/:id/messages — написать клиенту через бота;
//	POST /api/leads/:id/invoice  — выставить счёт CryptoBot, ссылка — клиенту.
//
// Порядок в POST messages принципиален: Sender.Send → CreateOutbound → WS.
// Telegram отказал (лид заблокировал бота и т.п.) → 502 ERR_TELEGRAM_SEND и
// в БД НИЧЕГО не пишем — в истории не должно быть недоставленных реплик.
// У счёта наоборот: инвойс уже создан, поэтому отказ Telegram → 502 с url
// в теле — счёт живой, менеджер перешлёт ссылку сам (сообщение не пишем).
package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/payment"
	"github.com/interfin/interfin-ai-crm/internal/repo"
	"github.com/interfin/interfin-ai-crm/internal/settings"
)

// maxMessageRunes — лимит длины текстового сообщения Telegram (4096 символов).
const maxMessageRunes = 4096

// invoiceExpiresIn — время жизни счёта из карточки: 24 часа (task M12 §4).
const invoiceExpiresIn = int(24 * time.Hour / time.Second)

// maxInvoiceDescriptionRunes — лимит description Crypto Pay createInvoice.
const maxInvoiceDescriptionRunes = 1024

// invoiceAssets — валюты счёта из карточки (task M12: USDT | USDC).
var invoiceAssets = map[string]bool{"USDT": true, "USDC": true}

// TelegramSender — отправка текста лиду (worker.TelebotSender в проде,
// фейк в тестах). Узкий срез worker.Sender: Typing чату менеджера не нужен.
type TelegramSender interface {
	Send(chatID int64, text string) error
}

// InvoiceCreator — контракт payment.Client для тестов.
type InvoiceCreator interface {
	CreateInvoice(ctx context.Context, leadID int64, params payment.CreateInvoiceParams) (*payment.Invoice, error)
}

// ChatDeps — зависимости ручек чата M12.
type ChatDeps struct {
	Leads    repo.LeadRepo
	Msgs     repo.MessageRepo
	Sender   TelegramSender
	Invoices InvoiceCreator
	Pub      events.Publisher
	// Settings — M13 (автопилот hybrid): после успешной реплики менеджера
	// при dialog_mode='bot' Эмма ставится на паузу hybrid_pause_minutes.
	// nil — автопилот выключен (старые тесты M12).
	Settings settings.Reader
	// Panel — строковые ключи настроек (заготовка приветствия чата,
	// emma_panel.chat_greeting_text). nil — заготовки нет (старые тесты).
	Panel StringSettings
	Log   *slog.Logger
}

// StringSettings — читающий контракт строковых ключей settings (в проде
// settings.Service, в тестах — фейк). Зеркало worker.PanelSettings.
type StringSettings interface {
	String(ctx context.Context, key string) string
}

// ChatHandler — GET/POST /api/leads/:id/messages и POST /api/leads/:id/invoice.
type ChatHandler struct {
	deps ChatDeps
}

func NewChat(deps ChatDeps) *ChatHandler {
	return &ChatHandler{deps: deps}
}

// Register вешает роуты на уже защищённую группу /api.
func (h *ChatHandler) Register(api gin.IRouter) {
	api.GET("/leads/:id/messages", h.listMessages)
	api.POST("/leads/:id/messages", h.postMessage)
	api.POST("/leads/:id/invoice", h.postInvoice)
	api.GET("/chat/greeting", h.greeting)
}

// greeting — GET /api/chat/greeting: заготовка приветствия для кнопки в
// ChatPanel (правится во вкладке «Сценарий» панели Эммы). Пустой текст —
// фронт кнопку не рисует.
func (h *ChatHandler) greeting(c *gin.Context) {
	text := ""
	if h.deps.Panel != nil {
		text = h.deps.Panel.String(c.Request.Context(), settings.KeyChatGreetingText)
	}
	c.JSON(http.StatusOK, gin.H{"text": text})
}

// listMessages — GET /api/leads/:id/messages?limit&before_id: страница
// истории от старых к новым (порядок контекста Claude); before_id —
// прокрутка вверх (id старейшего уже загруженного сообщения).
// Стёртый лид → 404, как остальные ручки M8.
func (h *ChatHandler) listMessages(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	limit := defaultPageLimit
	if raw := c.Query("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 || v > maxPageLimit {
			apiError(c, http.StatusBadRequest, "limit должен быть в 1..200", codeValidation)
			return
		}
		limit = v
	}
	var beforeID int64
	if raw := c.Query("before_id"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v <= 0 {
			apiError(c, http.StatusBadRequest,
				"before_id должен быть положительным числом", codeValidation)
			return
		}
		beforeID = v
	}

	if _, ok := h.lead(c, id); !ok {
		return
	}
	msgs, err := h.deps.Msgs.ListByLeadBefore(c.Request.Context(), id, beforeID, limit)
	if err != nil {
		h.internalError(c, "chat: list messages", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"messages": toMessageDTOs(msgs)})
}

type postMessageRequest struct {
	Text string `json:"text"`
}

// postMessage — POST /api/leads/:id/messages: реплика менеджера клиенту
// через бота. Send → CreateOutbound(author=manager:<sub>) → событие WS.
func (h *ChatHandler) postMessage(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var req postMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apiError(c, http.StatusBadRequest, "требуется поле text", codeValidation)
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		apiError(c, http.StatusBadRequest, "text пуст", codeValidation)
		return
	}
	if utf8.RuneCountInString(text) > maxMessageRunes {
		apiError(c, http.StatusBadRequest,
			fmt.Sprintf("text длиннее лимита Telegram %d символов", maxMessageRunes), codeValidation)
		return
	}
	lead, ok := h.lead(c, id)
	if !ok {
		return
	}

	// Сначала Telegram: недоставленная реплика в историю не попадает.
	if err := h.deps.Sender.Send(lead.TelegramUserID, text); err != nil {
		h.deps.Log.Warn("chat: telegram отказал", "lead_id", id, "error", err)
		apiError(c, http.StatusBadGateway,
			"Telegram не принял сообщение (лид мог заблокировать бота)", codeTelegramSend)
		return
	}
	msg, ok := h.saveOutbound(c, lead, text)
	if !ok {
		return
	}
	h.pauseBotAfterManagerReply(c, lead)
	c.JSON(http.StatusOK, gin.H{"message": toMessageDTO(msg)})
}

type postInvoiceRequest struct {
	Amount      string `json:"amount"`
	Asset       string `json:"asset"`
	Description string `json:"description"`
}

// postInvoice — POST /api/leads/:id/invoice: счёт CryptoBot из карточки.
// Контур M6 переиспользуется целиком: payload=lead_id кладёт payment.Client,
// оплата двигает карточку вебхуком M6 сама.
func (h *ChatHandler) postInvoice(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var req postInvoiceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apiError(c, http.StatusBadRequest, "требуются поля amount и asset", codeValidation)
		return
	}
	// amount — строка → decimal, как весь M6 (крипто-суммы во float нельзя).
	amount, err := decimal.NewFromString(strings.TrimSpace(req.Amount))
	if err != nil || !amount.IsPositive() {
		apiError(c, http.StatusBadRequest,
			"amount должен быть положительным числом в строке", codeValidation)
		return
	}
	asset := strings.ToUpper(strings.TrimSpace(req.Asset))
	if !invoiceAssets[asset] {
		apiError(c, http.StatusBadRequest, "asset должен быть USDT или USDC", codeValidation)
		return
	}
	description := strings.TrimSpace(req.Description)
	if utf8.RuneCountInString(description) > maxInvoiceDescriptionRunes {
		apiError(c, http.StatusBadRequest,
			fmt.Sprintf("description длиннее %d символов", maxInvoiceDescriptionRunes), codeValidation)
		return
	}
	lead, ok := h.lead(c, id)
	if !ok {
		return
	}

	inv, err := h.deps.Invoices.CreateInvoice(c.Request.Context(), id, payment.CreateInvoiceParams{
		Asset:       asset,
		Amount:      amount,
		Description: description,
		ExpiresIn:   invoiceExpiresIn,
	})
	if err != nil {
		h.deps.Log.Error("chat: createInvoice", "lead_id", id, "error", err)
		apiError(c, http.StatusBadGateway, "платёжный шлюз не создал счёт", codeInvoiceCreate)
		return
	}

	text := invoiceMessage(amount, asset, description, inv.BotInvoiceURL)
	if err := h.deps.Sender.Send(lead.TelegramUserID, text); err != nil {
		// Счёт уже живой — отдаём url менеджеру, он перешлёт ссылку сам.
		// Сообщение в БД не пишем: в истории нет недоставленных реплик.
		h.deps.Log.Warn("chat: счёт создан, telegram отказал",
			"lead_id", id, "invoice_id", inv.InvoiceID, "error", err)
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{
			"error":      "счёт создан, но Telegram не принял сообщение — перешлите ссылку сами",
			"code":       codeTelegramSend,
			"invoice_id": inv.InvoiceID,
			"url":        inv.BotInvoiceURL,
		})
		return
	}
	if _, ok := h.saveOutbound(c, lead, text); !ok {
		return
	}
	h.pauseBotAfterManagerReply(c, lead)
	c.JSON(http.StatusOK, gin.H{"invoice_id": inv.InvoiceID, "url": inv.BotInvoiceURL})
}

// pauseBotAfterManagerReply — автопилот hybrid (M13 §4): менеджер ответил
// из карточки при dialog_mode='bot' → Эмма молчит hybrid_pause_minutes
// (не перебивает живой разговор), затем включается сама. В режиме human
// пауза не трогается — там Эмму глушит сам режим. Best effort ПОСЛЕ
// доставки: реплика уже у клиента, ошибка паузы бизнес-ответ не роняет.
func (h *ChatHandler) pauseBotAfterManagerReply(c *gin.Context, lead *models.Lead) {
	if h.deps.Settings == nil || lead.DialogMode != models.DialogModeBot {
		return
	}
	ctx := c.Request.Context()
	pause := time.Duration(h.deps.Settings.Minutes(ctx, settings.KeyHybridPauseMinutes)) * time.Minute
	until := time.Now().UTC().Add(pause)
	if err := h.deps.Leads.UpdateFields(ctx, lead.ID,
		map[string]interface{}{"bot_silenced_until": until}); err != nil {
		h.deps.Log.Error("chat: пауза автопилота не выставлена",
			"lead_id", lead.ID, "error", err)
		return
	}
	lead.BotSilencedUntil = &until
	if err := h.deps.Pub.Publish(ctx,
		events.DialogModeEvent(lead, "autopilot pause after manager reply")); err != nil {
		h.deps.Log.Warn("chat: событие dialog_mode не опубликовано",
			"lead_id", lead.ID, "error", err)
	}
}

// invoiceMessage — короткий текст от Эммы со ссылкой на оплату (task M12 §4).
func invoiceMessage(amount decimal.Decimal, asset, description, url string) string {
	var b strings.Builder
	b.WriteString("Я подготовила для вас счёт на ")
	b.WriteString(amount.String())
	b.WriteString(" ")
	b.WriteString(asset)
	if description != "" {
		b.WriteString(" — ")
		b.WriteString(description)
	}
	b.WriteString(".\nОплатить можно по ссылке (действует 24 часа):\n")
	b.WriteString(url)
	return b.String()
}

// lead — лид по id; стёртый/несуществующий → 404 (false = ответ уже отправлен).
func (h *ChatHandler) lead(c *gin.Context, id int64) (*models.Lead, bool) {
	lead, err := h.deps.Leads.GetByID(c.Request.Context(), id)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		apiError(c, http.StatusNotFound, "лид не найден", codeNotFound)
		return nil, false
	case err != nil:
		h.internalError(c, "chat: get lead", err)
		return nil, false
	}
	return lead, true
}

// saveOutbound — доставленная реплика менеджера: строка в messages
// (author=manager:<sub>, счётчики лида не трогаются — CLAUDE.md §4.3)
// + событие message в crm:events (третья точка публикации M12).
func (h *ChatHandler) saveOutbound(c *gin.Context, lead *models.Lead, text string) (*models.Message, bool) {
	author := managerAuthor(c)
	msg := &models.Message{LeadID: lead.ID, Author: &author, Content: text}
	if err := h.deps.Msgs.CreateOutbound(c.Request.Context(), msg); err != nil {
		// Сообщение уже у лида, а строки нет — потеря истории, кричим в лог.
		h.internalError(c, "chat: outbound отправлен, но не сохранён", err)
		return nil, false
	}
	// Fire-and-forget, как все crm:events: пропуск фронт добирает
	// перезапросом истории при reconnect (§10.3).
	if err := h.deps.Pub.Publish(c.Request.Context(), events.MessageEvent(msg, lead.StageID)); err != nil {
		h.deps.Log.Warn("chat: событие message не опубликовано",
			"lead_id", lead.ID, "error", err)
	}
	return msg, true
}

// managerAuthor — messages.author из JWT claims: manager:<sub> (0012).
func managerAuthor(c *gin.Context) string {
	if claims, ok := auth.ClaimsFrom(c); ok {
		return models.AuthorManagerPrefix + claims.Subject
	}
	return "manager" // недостижимо за auth.Middleware; страховка для тестов
}

func (h *ChatHandler) internalError(c *gin.Context, what string, err error) {
	h.deps.Log.Error(what, "error", err)
	apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
}
