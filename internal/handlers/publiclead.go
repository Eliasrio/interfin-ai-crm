// publiclead.go — публичный приём заявок с сайта borninbrazil.baby.
//
// POST /api/public/lead — БЕЗ auth (форма сайта шлёт fetch'ем): группа
// /api/public живёт рядом с /api, JWT-гейт M8 на неё не распространяется.
// Защита: CORS-allowlist origin'ов + свой rate limit по IP (ключи
// «public:<ip>» — бюджет /api не задевается) + лимит тела 16КБ.
//
// Веб-лид не имеет Telegram-чата, а leads.telegram_user_id NOT NULL + UNIQUE
// по живым строкам (0003/0011). Поэтому идентификатор синтетический:
// ОТРИЦАТЕЛЬНЫЙ int64 из SHA-256 нормализованного телефона (приём M8 §9.3 —
// хеши erasure тоже отрицательные, живые Telegram ID всегда положительные,
// коллизий нет). Детерминированность даёт дедуп: повторная заявка с тем же
// телефоном не плодит карточку, а дописывает сообщение в существующую.
//
// Содержимое формы сохраняется inbound-сообщением (это и есть входящее
// обращение лида — счётчики двигает repo.CreateInbound, CLAUDE.md §4.3),
// карточка всплывает на доске событием message (fetchUnknownLead §10.3),
// менеджер дополнительно получает уведомление в ManagerChatID. В очередь
// process:inbound задача НЕ ставится: Эмме некуда отвечать (chatID
// синтетический) — диалог ведёт менеджер по телефону/мессенджеру.
package handlers

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
	"github.com/interfin/interfin-ai-crm/internal/settings"
)

// publicLeadMaxBody — предохранитель от гигантских тел: форма сайта —
// сотни байт, 16КБ хватает с запасом.
const publicLeadMaxBody = 16 << 10

// Лимиты полей заявки. Телефон и имя валидируются жёстко (400 — сайт
// откатится на mailto), остальные поля молча обрезаются: потерять хвост
// длинного комментария лучше, чем потерять заявку целиком.
const (
	publicLeadMaxNameRunes  = 255 // leads.name VARCHAR(255)
	publicLeadMaxPhoneRunes = 64  // сырой ввод; в leads.phone уходит обрезка до 32 (VARCHAR(32))
	publicLeadPhoneColRunes = 32
	publicLeadMinPhoneDigit = 6 // меньше — не телефон, а мусор/спам
	publicLeadMaxFieldRunes = 300
	publicLeadMaxMsgRunes   = 2000
)

// PublicLeadDeps — зависимости публичного приёма заявок.
type PublicLeadDeps struct {
	Leads  repo.LeadRepo
	Msgs   repo.MessageRepo
	Sender TelegramSender   // уведомление менеджеру; тот же Sender, что у чата M12
	Pub    events.Publisher // событие message — карточка всплывает на доске; nil — без публикации
	// Limiter — свой лимитер заявок (ключи public:<ip>). nil — лимит выключен
	// (юнит-тестовые конфиги).
	Limiter RateLimiter
	// AllowedOrigins — CORS-allowlist сайта (config public_lead.allowed_origins).
	AllowedOrigins []string
	// ManagerChatID — чат уведомлений (тот же, что dead letter §6.3). 0 —
	// уведомления остаются в логе.
	ManagerChatID int64
	// PublicURL — база deep-link'ов на карточку (как у worker/takeover).
	PublicURL string
	// Panel — @упоминание менеджера (emma_panel.manager_mention) в
	// уведомлении, как у handoff/takeover. nil — без упоминания (тесты).
	Panel StringSettings
	Log   *slog.Logger
}

// PublicLeadHandler — POST /api/public/lead.
type PublicLeadHandler struct {
	deps    PublicLeadDeps
	origins map[string]bool
}

func NewPublicLead(deps PublicLeadDeps) *PublicLeadHandler {
	origins := make(map[string]bool, len(deps.AllowedOrigins))
	for _, o := range deps.AllowedOrigins {
		origins[normalizeOrigin(o)] = true
	}
	return &PublicLeadHandler{deps: deps, origins: origins}
}

// Register вешает группу /api/public на КОРНЕВОЙ роутер (не на группу /api —
// иначе заявка упёрлась бы в JWT). Rate limit — только на POST: preflight
// OPTIONS браузер шлёт перед каждой отправкой, он не должен съедать бюджет.
func (h *PublicLeadHandler) Register(r gin.IRouter) {
	g := r.Group("/api/public", h.cors())
	// Явный OPTIONS-роут обязателен: без него gin отдал бы 404 до middleware.
	// Сам ответ формирует cors() (204 + заголовки); хендлер — недостижимая
	// страховка для запросов без Origin.
	g.OPTIONS("/lead", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	g.POST("/lead", h.rateLimit(), h.postLead)
}

// cors — CORS-allowlist формы сайта. Запросы без Origin (curl, мониторинг)
// пропускаются: CORS — защита браузера, а не сервера; сервер защищают
// rate limit и валидация. Чужой Origin — 403 (defense in depth: браузер и
// так не отдаст ответ, но мы не делаем и работу).
func (h *PublicLeadHandler) cors() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			c.Next()
			return
		}
		if !h.origins[normalizeOrigin(origin)] {
			apiError(c, http.StatusForbidden, "origin не разрешён", codeOriginForbidden)
			return
		}
		hdr := c.Writer.Header()
		hdr.Set("Access-Control-Allow-Origin", origin)
		hdr.Add("Vary", "Origin")
		if c.Request.Method == http.MethodOptions {
			hdr.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			hdr.Set("Access-Control-Allow-Headers", "Content-Type")
			hdr.Set("Access-Control-Max-Age", "86400")
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// rateLimit — свой лимит заявок по IP. От RateLimit (§4.2) отличается ключом
// public:<ip> (отдельный бюджет) и источником IP: X-Real-IP от nginx, иначе
// RemoteAddr. c.ClientIP() не используется сознательно — gin по умолчанию
// доверяет X-Forwarded-For, который nginx НЕ перезаписывает, т.е. на
// публичном эндпоинте клиент мог бы назначать себе IP сам.
func (h *PublicLeadHandler) rateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.deps.Limiter == nil {
			c.Next()
			return
		}
		ip := publicClientIP(c)
		ok, err := h.deps.Limiter.Allow(c.Request.Context(), "public:"+ip)
		if err != nil {
			// Fail-open, как RateLimit §4.2: минуту без лимита лучше,
			// чем потерянные заявки из-за упавшего Redis.
			h.deps.Log.Warn("public lead: лимитер недоступен, запрос пропущен без лимита",
				"ip", ip, "error", err)
			c.Next()
			return
		}
		if !ok {
			apiError(c, http.StatusTooManyRequests,
				"слишком много заявок с этого IP, попробуйте позже", codeRateLimited)
			return
		}
		c.Next()
	}
}

// publicLeadRequest — контракт формы сайта (index.html: FormData + lang/source).
type publicLeadRequest struct {
	Name      string `json:"name"`
	Phone     string `json:"phone"`
	Messenger string `json:"messenger"`
	DueDate   string `json:"due_date"`
	City      string `json:"city"`
	Package   string `json:"package"`
	Message   string `json:"message"`
	Lang      string `json:"lang"`
	Source    string `json:"source"`
}

// postLead — ядро: валидация → лид (создать/найти по хешу телефона) →
// inbound-сообщение с текстом заявки → событие message → уведомление
// менеджеру. Сайт считает успехом любой 2xx, иначе откатывается на mailto —
// поэтому 200 отдаётся, если заявка ГДЕ-ТО зафиксирована: в БД (карточка на
// доске) или хотя бы в Telegram менеджера (БД лежит, но данные не потеряны).
func (h *PublicLeadHandler) postLead(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, publicLeadMaxBody)
	var req publicLeadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apiError(c, http.StatusBadRequest, "невалидный JSON заявки", codeValidation)
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.Phone = strings.TrimSpace(req.Phone)
	switch {
	case req.Name == "" || req.Phone == "":
		apiError(c, http.StatusBadRequest, "name и phone обязательны", codeValidation)
		return
	case utf8.RuneCountInString(req.Name) > publicLeadMaxNameRunes:
		apiError(c, http.StatusBadRequest, "name слишком длинный", codeValidation)
		return
	case utf8.RuneCountInString(req.Phone) > publicLeadMaxPhoneRunes:
		apiError(c, http.StatusBadRequest, "phone слишком длинный", codeValidation)
		return
	}
	digits := phoneDigits(req.Phone)
	if len(digits) < publicLeadMinPhoneDigit {
		apiError(c, http.StatusBadRequest, "phone не похож на телефон", codeValidation)
		return
	}
	// Необязательные поля: обрезка вместо 400 — заявку не теряем.
	req.Messenger = truncateRunes(strings.TrimSpace(req.Messenger), publicLeadMaxFieldRunes)
	req.DueDate = truncateRunes(strings.TrimSpace(req.DueDate), publicLeadMaxFieldRunes)
	req.City = truncateRunes(strings.TrimSpace(req.City), publicLeadMaxFieldRunes)
	req.Package = truncateRunes(strings.TrimSpace(req.Package), publicLeadMaxFieldRunes)
	req.Message = truncateRunes(strings.TrimSpace(req.Message), publicLeadMaxMsgRunes)
	req.Lang = strings.TrimSpace(req.Lang)
	req.Source = truncateRunes(strings.TrimSpace(req.Source), publicLeadMaxFieldRunes)

	ctx := c.Request.Context()
	lead, created, err := h.findOrCreateLead(c, &req, digits)
	if err != nil {
		// БД лежит: последняя линия — заявка текстом в чат менеджера.
		h.deps.Log.Error("public lead: лид не сохранён", "error", err)
		if h.notifyManager(c, &req, nil, false) {
			c.JSON(http.StatusOK, gin.H{"ok": true})
			return
		}
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}

	inbound := &models.Message{LeadID: lead.ID, Content: publicLeadText(&req)}
	if err := h.deps.Msgs.CreateInbound(ctx, inbound); err != nil {
		// Карточка есть, а деталей в чате нет — кричим в лог; уведомление
		// ниже всё равно унесёт менеджеру полный текст заявки.
		h.deps.Log.Error("public lead: сообщение заявки не сохранено",
			"lead_id", lead.ID, "error", err)
	} else if h.deps.Pub != nil {
		// Fire-and-forget, как все crm:events: доска без события добирает
		// лида catch-up'ом GET /api/leads?updated_since (§10.3).
		if err := h.deps.Pub.Publish(ctx, events.MessageEvent(inbound, lead.StageID)); err != nil {
			h.deps.Log.Warn("public lead: событие message не опубликовано",
				"lead_id", lead.ID, "error", err)
		}
	}

	h.notifyManager(c, &req, lead, created)
	h.deps.Log.Info("public lead: заявка принята",
		"lead_id", lead.ID, "created", created, "source", req.Source)
	c.JSON(http.StatusOK, gin.H{"ok": true, "lead_id": lead.ID})
}

// findOrCreateLead — лид по синтетическому telegram_user_id (хеш телефона):
// повторная заявка с тем же номером попадает в существующую карточку.
// Гонку двух первых заявок разруливает частичный UNIQUE (0011): проигравший
// Create перечитывает лида — как findOrCreateLead вебхука M2.
func (h *PublicLeadHandler) findOrCreateLead(c *gin.Context, req *publicLeadRequest, digits string) (*models.Lead, bool, error) {
	ctx := c.Request.Context()
	tgID := webLeadTelegramUserID(digits)
	lead, err := h.deps.Leads.GetByTelegramUserID(ctx, tgID)
	if err == nil {
		return lead, false, nil
	}
	if !errors.Is(err, repo.ErrNotFound) {
		return nil, false, err
	}

	now := time.Now()
	phone := truncateRunes(req.Phone, publicLeadPhoneColRunes)
	lead = &models.Lead{
		TelegramUserID: tgID,
		StageID:        1, // новый лид всегда входит в первую стадию Kanban
		LastActivityAt: now,
		Name:           &req.Name,
		Phone:          &phone,
		// Отправка формы = согласие на контакт (LGPD): единственная точка
		// входа лида, где согласие явное действие, а не факт переписки.
		ConsentGivenAt: &now,
	}
	if l := req.Lang; l == "ru" || l == "en" || l == "es" {
		// Язык версии сайта — язык лида (CHECK 0015 допускает только эти три).
		lang := l
		lead.Language = &lang
	}
	if err := h.deps.Leads.Create(ctx, lead); err != nil {
		if existing, gerr := h.deps.Leads.GetByTelegramUserID(ctx, tgID); gerr == nil {
			return existing, false, nil
		}
		return nil, false, err
	}
	h.deps.Log.Info("public lead: создан новый лид", "lead_id", lead.ID)
	return lead, true, nil
}

// notifyManager — заявка текстом в ManagerChatID (best effort, как алерты
// dead letter §6.3). lead nil — БД лежала, уведомление становится
// единственным носителем заявки: успех возвращается вызывающему.
func (h *PublicLeadHandler) notifyManager(c *gin.Context, req *publicLeadRequest, lead *models.Lead, created bool) bool {
	if h.deps.ManagerChatID == 0 {
		return false
	}
	ctx := c.Request.Context()
	mention := ""
	if h.deps.Panel != nil {
		mention = mentionPrefixPublic(h.deps.Panel.String(ctx, settings.KeyManagerMention))
	}
	var b strings.Builder
	b.WriteString(mention)
	switch {
	case lead == nil:
		b.WriteString("🌐 Заявка с сайта — CRM не сохранила её, данные только здесь!\n")
	case created:
		b.WriteString("🌐 Новая заявка с сайта\n")
	default:
		b.WriteString("🌐 Повторная заявка с сайта (карточка уже была)\n")
	}
	b.WriteString(publicLeadText(req))
	b.WriteString("\n\n⚠️ Telegram-чата у лида нет — свяжитесь по телефону/мессенджеру.")
	if lead != nil {
		if link := publicLeadCardURL(h.deps.PublicURL, lead.ID); link != "" {
			b.WriteString("\nОткрыть карточку: " + link)
		}
	}
	if err := h.deps.Sender.Send(h.deps.ManagerChatID, b.String()); err != nil {
		h.deps.Log.Error("public lead: уведомление менеджеру не доставлено", "error", err)
		return false
	}
	return true
}

// publicLeadText — человекочитаемый текст заявки: и inbound-сообщение
// карточки, и тело уведомления менеджеру (один текст — нечему разъезжаться).
func publicLeadText(req *publicLeadRequest) string {
	source := req.Source
	if source == "" {
		source = "сайт"
	}
	var b strings.Builder
	b.WriteString("Заявка с " + source)
	line := func(label, v string) {
		if v != "" {
			b.WriteString("\n" + label + ": " + v)
		}
	}
	line("Имя", req.Name)
	line("Телефон", req.Phone)
	line("Мессенджер", req.Messenger)
	line("Срок родов", req.DueDate)
	line("Город", req.City)
	line("Пакет", req.Package)
	line("Комментарий", req.Message)
	line("Язык сайта", req.Lang)
	return b.String()
}

// publicLeadCardURL — deep-link на карточку лида (?lead=<id>, App.jsx),
// зеркало worker.leadCardURL. base пуст — ссылки нет.
func publicLeadCardURL(base string, id int64) string {
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/?lead=" + strconv.FormatInt(id, 10)
}

// webLeadTelegramUserID — синтетический telegram_user_id веб-лида:
// ОТРИЦАТЕЛЬНЫЙ int64 из первых 8 байт SHA-256("weblead:"+цифры телефона).
// Приём зеркалит lgpd.HashTelegramUserID (M8 §9.3): живые Telegram ID
// положительные — коллизия исключена; частичный UNIQUE (0011) смотрит только
// на живые строки, так что стёртые erasure-хеши тоже не мешают.
func webLeadTelegramUserID(phoneDigits string) int64 {
	sum := sha256.Sum256([]byte("weblead:" + phoneDigits))
	v := int64(binary.BigEndian.Uint64(sum[:8]) &^ (1 << 63)) // бит знака в 0: v >= 0
	if v == 0 {
		v = 1
	}
	return -v
}

// phoneDigits — нормализация телефона для хеша: только цифры, чтобы
// «+55 (21) 99999-9999» и «5521999999999» дедуплицировались в один лид.
func phoneDigits(phone string) string {
	var b strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// publicClientIP — IP клиента для rate limit: X-Real-IP (его ставит наш
// nginx и клиент подделать не может), иначе RemoteAddr соединения.
func publicClientIP(c *gin.Context) string {
	if ip := strings.TrimSpace(c.GetHeader("X-Real-IP")); ip != "" && net.ParseIP(ip) != nil {
		return ip
	}
	return c.RemoteIP()
}

// normalizeOrigin — сравнение Origin без учёта регистра и хвостового «/».
func normalizeOrigin(origin string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(origin), "/"))
}

// truncateRunes — первые max рун строки (лимиты полей заявки; по рунам,
// чтобы не порвать кириллицу на границе байта).
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max])
}

// mentionPrefixPublic — «@username » перед уведомлением (зеркало
// worker.mentionPrefix: пакеты не импортируют друг друга ради 5 строк).
func mentionPrefixPublic(mention string) string {
	m := strings.TrimSpace(mention)
	if m == "" {
		return ""
	}
	return "@" + strings.TrimPrefix(m, "@") + " "
}
