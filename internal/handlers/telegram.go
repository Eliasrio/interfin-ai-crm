// Package handlers — HTTP-обработчики бизнес-уровня (M2: Telegram ingestion).
//
// Цепочка POST /webhook/telegram (SRS §6.1, §5.4):
//
//	SecretToken → SaveAndReturn200 → gin.WrapH(webhook telebot)
//
// Гарантия контракта M2: входящее сообщение сохранено в messages ДО ответа 200.
// Всё тяжёлое (Claude — 3–15 с) сюда не заходит: телеграмный таймаут 5 с,
// поэтому здесь только «сохранить + поставить в очередь» (CLAUDE.md §4.4).
package handlers

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	tele "gopkg.in/telebot.v3"

	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/lang"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// SecretTokenHeader — заголовок, которым Telegram подписывает каждый запрос
// к webhook, если при setWebhook передан secret_token (SRS §5.4).
const SecretTokenHeader = "X-Telegram-Bot-Api-Secret-Token"

// maxBodyBytes — предохранитель от гигантских тел. Апдейт Telegram —
// килобайты; 1 МБ хватает с запасом.
const maxBodyBytes = 1 << 20

// TelegramWebhook — зависимости ingestion-цепочки.
type TelegramWebhook struct {
	leads  repo.LeadRepo
	msgs   repo.MessageRepo
	queue  queue.Enqueuer
	pub    events.Publisher // M12: событие message на каждый inbound; nil — без публикации (тесты M2)
	secret string
	log    *slog.Logger
}

func NewTelegramWebhook(
	leads repo.LeadRepo,
	msgs repo.MessageRepo,
	q queue.Enqueuer,
	pub events.Publisher,
	secret string,
	log *slog.Logger,
) *TelegramWebhook {
	return &TelegramWebhook{leads: leads, msgs: msgs, queue: q, pub: pub, secret: secret, log: log}
}

// Register вешает цепочку на POST /webhook/telegram. dispatch — telebot-webhook
// (http.Handler) через gin.WrapH: в M2 у бота нет хендлеров, но регистрация
// обязательна по задаче §6.1 — M3 начнёт диспетчеризацию без изменений роутинга.
func (h *TelegramWebhook) Register(r gin.IRouter, dispatch http.Handler) {
	r.POST("/webhook/telegram", h.SecretToken(), h.SaveAndReturn200(), gin.WrapH(dispatch))
}

// SecretToken — middleware безопасности (SRS §5.4): сверяет заголовок
// Telegram с секретом из конфига; при невалидном → 403.
// Сам telebot при несовпадении отвечает 200 без обработки — этого мало,
// поэтому проверка живёт до WrapH.
func (h *TelegramWebhook) SecretToken() gin.HandlerFunc {
	return func(c *gin.Context) {
		got := c.GetHeader(SecretTokenHeader)
		// Пустой секрет в конфиге = сервер сконфигурирован небезопасно;
		// не превращаем это в «пускать всех».
		if h.secret == "" || subtle.ConstantTimeCompare([]byte(got), []byte(h.secret)) != 1 {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "invalid webhook secret token",
				"code":  "ERR_WEBHOOK_FORBIDDEN",
			})
			return
		}
		c.Next()
	}
}

// SaveAndReturn200 — ядро ingestion (задача M2 §4):
//
//  1. найти/создать лида по telegram_user_id;
//  2. сохранить сообщение direction='inbound' (message_count и
//     last_activity_at двигает repo.CreateInbound атомарно);
//  3. поставить process:inbound в Asynq с дедупликацией;
//  4. ответить 200 — Telegram свободен, остальное делает воркер (M3).
//
// Если Redis недоступен, сообщение уже в БД: помечаем лида pending_task=TRUE
// (graceful degradation, SRS §11.2) и всё равно отвечаем 200 — иначе Telegram
// будет ретраить апдейт, который мы уже сохранили.
func (h *TelegramWebhook) SaveAndReturn200() gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes))
		if err != nil {
			h.log.Error("webhook: чтение тела", "error", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"error": "cannot read request body",
				"code":  "ERR_WEBHOOK_READ",
			})
			return
		}
		// telebot в WrapH читает тело повторно — возвращаем его на место.
		c.Request.Body = io.NopCloser(bytes.NewReader(body))

		var upd tele.Update
		if err := json.Unmarshal(body, &upd); err != nil {
			// Секрет уже сверён, значит это Telegram; отвечаем 200, чтобы
			// не зациклить ретраи нечитаемого апдейта, и не будим telebot.
			h.log.Warn("webhook: нераспознанный апдейт", "error", err)
			c.AbortWithStatus(http.StatusOK)
			return
		}

		msg := upd.Message
		if msg == nil || msg.Sender == nil {
			// Не входящее сообщение лида (edited_message, callback, channel
			// post и т.п.) — M2 такие не сохраняет, telebot пусть смотрит сам.
			c.Status(http.StatusOK)
			c.Next()
			return
		}

		ctx := c.Request.Context()
		lead, err := h.findOrCreateLead(ctx, msg.Sender)
		if err != nil {
			h.abortSaveFailed(c, "webhook: лид не найден/не создан", err)
			return
		}

		// Текст либо подпись к медиа; для стикеров и т.п. сохраняем пустую
		// строку — гарантия «каждое входящее сохранено» важнее содержимого.
		inbound := &models.Message{
			LeadID:  lead.ID,
			Content: textOf(msg),
		}

		// M14: первый ТЕКСТОВЫЙ inbound лида без языка — детекция и запись
		// той же транзакцией, что и сообщение. Нетекстовое (content пуст)
		// язык не выставляет; детекция локальная — правило «200 немедленно»
		// (CLAUDE.md §4.4) не нарушается. Команды бота («/start» с кнопки
		// «Начать» — первое сообщение почти каждого лида) — не речь лида:
		// детектор увидел бы в них английский, язык ждёт настоящего текста.
		text := strings.TrimSpace(inbound.Content)
		if lead.Language == nil && text != "" && !strings.HasPrefix(text, "/") {
			detected := lang.Detect(inbound.Content)
			langSet, err := h.msgs.CreateInboundSetLanguage(ctx, inbound, detected)
			if err != nil {
				h.abortSaveFailed(c, "webhook: сообщение не сохранено", err,
					slog.Int64("lead_id", lead.ID))
				return
			}
			if langSet {
				lead.Language = &detected
				// Fire-and-forget, как всё в crm:events: пропуск клиент
				// добирает срезом GET /api/leads (§10.3).
				if h.pub != nil {
					if err := h.pub.Publish(ctx,
						events.LeadLanguageEvent(lead, "autodetect first inbound")); err != nil {
						h.log.Warn("webhook: событие lead_language не опубликовано",
							"lead_id", lead.ID, "error", err)
					}
				}
				h.log.Info("webhook: язык лида определён",
					"lead_id", lead.ID, "language", detected)
			}
		} else if err := h.msgs.CreateInbound(ctx, inbound); err != nil {
			h.abortSaveFailed(c, "webhook: сообщение не сохранено", err,
				slog.Int64("lead_id", lead.ID))
			return
		}

		// M12: чат менеджера живой в обе стороны — inbound уходит событием
		// message. Fire-and-forget, как всё в crm:events: пропуск клиент
		// добирает перезапросом истории при reconnect (§10.3).
		if h.pub != nil {
			if err := h.pub.Publish(ctx, events.MessageEvent(inbound, lead.StageID)); err != nil {
				h.log.Warn("webhook: событие message не опубликовано",
					"lead_id", lead.ID, "error", err)
			}
		}

		// Сообщение в БД — можно ставить задачу. Дедуп-ключ строится от
		// telegram message_id: повторная доставка апдейта даёт ErrDuplicate.
		switch err := h.queue.EnqueueInbound(ctx, lead.ID, msg.ID); {
		case errors.Is(err, queue.ErrDuplicate):
			h.log.Info("webhook: дубль апдейта, задача уже в очереди",
				"lead_id", lead.ID, "tg_msg_id", msg.ID)
		case err != nil:
			// Redis down: НЕ 5xx. Сообщение сохранено, лида помечаем —
			// recovery-механизм перевыставит задачу (SRS §11.2).
			h.log.Error("webhook: enqueue не удался, ставим pending_task",
				"lead_id", lead.ID, "error", err)
			if uerr := h.leads.UpdateFields(ctx, lead.ID,
				map[string]interface{}{"pending_task": true}); uerr != nil {
				h.log.Error("webhook: pending_task не выставлен",
					"lead_id", lead.ID, "error", uerr)
			}
		}

		// 200 немедленно (CLAUDE.md §4.4): статус зафиксирован, WrapH дальше
		// лишь раскладывает апдейт по хендлерам telebot (в M2 их нет).
		c.Status(http.StatusOK)
		c.Next()
	}
}

// findOrCreateLead возвращает лида по telegram_user_id, при первом контакте —
// создаёт. Гонку двух первых апдейтов разруливает UNIQUE(telegram_user_id):
// проигравший Create перечитывает лида.
func (h *TelegramWebhook) findOrCreateLead(ctx context.Context, from *tele.User) (*models.Lead, error) {
	lead, err := h.leads.GetByTelegramUserID(ctx, from.ID)
	if err == nil {
		return lead, nil
	}
	if !errors.Is(err, repo.ErrNotFound) {
		return nil, err
	}

	lead = &models.Lead{
		TelegramUserID: from.ID,
		StageID:        1, // новый лид всегда входит в первую стадию Kanban
		LastActivityAt: time.Now(),
	}
	if name := fullName(from); name != "" {
		lead.Name = &name
	}
	if from.Username != "" {
		username := from.Username
		lead.TgUsername = &username
	}

	if err := h.leads.Create(ctx, lead); err != nil {
		// Возможный проигрыш гонки создания — пробуем перечитать.
		if existing, gerr := h.leads.GetByTelegramUserID(ctx, from.ID); gerr == nil {
			return existing, nil
		}
		return nil, err
	}
	h.log.Info("webhook: создан новый лид", "lead_id", lead.ID)
	return lead, nil
}

// abortSaveFailed — сохранить не смогли, значит 200 отдавать нельзя:
// пусть Telegram ретраит апдейт (контракт: сохранено ДО 200).
func (h *TelegramWebhook) abortSaveFailed(c *gin.Context, msg string, err error, attrs ...slog.Attr) {
	h.log.LogAttrs(c.Request.Context(), slog.LevelError, msg,
		append(attrs, slog.Any("error", err))...)
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
		"error": "failed to persist inbound message",
		"code":  "ERR_WEBHOOK_PERSIST",
	})
}

func textOf(msg *tele.Message) string {
	if msg.Text != "" {
		return msg.Text
	}
	return msg.Caption
}

func fullName(u *tele.User) string {
	switch {
	case u.FirstName != "" && u.LastName != "":
		return u.FirstName + " " + u.LastName
	case u.FirstName != "":
		return u.FirstName
	default:
		return u.LastName
	}
}
