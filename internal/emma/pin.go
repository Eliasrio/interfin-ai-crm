// pin.go — роуты /api/emma/pin/* (EP-01, ТЗ §2.2/§6). Группа стоит за
// auth + RequireRole(admin), но БЕЗ RequirePIN — иначе некому было бы
// установить первый PIN (исправление v1.0 ТЗ).
//
// Наружу статусы «PIN не задан» и «PIN неверен» не различаются — оба
// 401 PIN_INVALID (утечка «PIN ещё не установлен» — подарок брутфорсу);
// «не задан» честно отдаёт только pin/status владельцу-админу.
package emma

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/settings"
)

// Коды ошибок контура PIN — контракт для фронта EP-07.
const (
	CodePinRequired    = "PIN_REQUIRED"    // 401: нет PIN-сессии на защищённой группе
	CodePinInvalid     = "PIN_INVALID"     // 401: PIN неверен ЛИБО не задан (не различаем)
	CodePinLocked      = "PIN_LOCKED"      // 429: ≥5 промахов, ждать retry_after сек
	CodePinAlreadySet  = "PIN_ALREADY_SET" // 409: повторный setup
	CodePinUnavailable = "PIN_UNAVAILABLE" // 503: Redis недоступен — fail-closed §2.2
	codeValidation     = "ERR_VALIDATION"  // 400: формат тела/PIN (конвенция §4.2)
	codeInternal       = "ERR_INTERNAL"    // 500: ошибка записи settings
)

// bcryptCost — стоимость хэша PIN (ТЗ §2.2). Дороже дефолтных 10:
// PIN — 6 цифр, перебор оффлайн-дампа дешевле пароля.
const bcryptCost = 12

// pinRe — ровно 6 цифр (решение владельца: 6, не 4 как в v1.0).
var pinRe = regexp.MustCompile(`^\d{6}$`)

// Settings — нужный emma срез настроек (боевой — settings.Service).
type Settings interface {
	String(ctx context.Context, key string) string
	SetString(ctx context.Context, key, val string) error
}

// PINDeps — зависимости роутов pin/*.
type PINDeps struct {
	Settings Settings
	Store    Store
	Log      *slog.Logger
}

// PINHandler — GET status, POST setup/verify/change, DELETE session.
type PINHandler struct {
	deps PINDeps
}

func NewPIN(deps PINDeps) *PINHandler {
	return &PINHandler{deps: deps}
}

// Register вешает роуты на группу /api/emma/pin (auth + admin, без RequirePIN).
func (h *PINHandler) Register(g gin.IRouter) {
	g.GET("/status", h.status)
	g.POST("/setup", h.setup)
	g.POST("/verify", h.verify)
	g.POST("/change", h.change)
	g.DELETE("/session", h.closeSession)
}

// apiError — формат ошибок CLAUDE.md §5, как handlers.apiError.
func apiError(c *gin.Context, status int, msg, code string) {
	c.AbortWithStatusJSON(status, gin.H{"error": msg, "code": code})
}

// managerID — sub из JWT-claims (id менеджера строкой, контракт M7).
func managerID(c *gin.Context) (string, bool) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok || claims.Subject == "" {
		// Достижимо только при сборке роутера без auth.Middleware.
		apiError(c, http.StatusUnauthorized, "требуется аутентификация", "ERR_TOKEN_INVALID")
		return "", false
	}
	return claims.Subject, true
}

// pinSet — задан ли PIN. settings.String при недоступной БД отдаёт дефолт
// "" — контур закрывается в сторону «PIN не задан» (setup упадёт на записи,
// verify ответит PIN_INVALID; наружу ничего не открывается).
func (h *PINHandler) pinSet(ctx context.Context) bool {
	return h.deps.Settings.String(ctx, settings.KeyPinHash) != ""
}

// GET /api/emma/pin/status → {pin_set, session_active}
func (h *PINHandler) status(c *gin.Context) {
	sub, ok := managerID(c)
	if !ok {
		return
	}
	active, err := h.deps.Store.SessionActive(c.Request.Context(), sub)
	if err != nil {
		h.failClosed(c, "pin/status", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"pin_set":        h.pinSet(c.Request.Context()),
		"session_active": active,
	})
}

type pinBody struct {
	PIN string `json:"pin"`
}

type changeBody struct {
	OldPIN string `json:"old_pin"`
	NewPIN string `json:"new_pin"`
}

// POST /api/emma/pin/setup {pin} — первичная установка (bootstrap ТЗ §2.2).
// После установки сессия открывается сразу — владельцу не вводить PIN дважды.
func (h *PINHandler) setup(c *gin.Context) {
	sub, ok := managerID(c)
	if !ok {
		return
	}
	var req pinBody
	if err := c.ShouldBindJSON(&req); err != nil || !pinRe.MatchString(req.PIN) {
		apiError(c, http.StatusBadRequest, "pin — ровно 6 цифр", codeValidation)
		return
	}
	ctx := c.Request.Context()
	if h.pinSet(ctx) {
		apiError(c, http.StatusConflict,
			"PIN уже установлен — меняется через pin/change, сбрасывается с сервера", CodePinAlreadySet)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.PIN), bcryptCost)
	if err != nil {
		h.deps.Log.Error("emma: pin setup: bcrypt", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	if err := h.deps.Settings.SetString(ctx, settings.KeyPinHash, string(hash)); err != nil {
		h.deps.Log.Error("emma: pin setup: сохранение хэша", "manager_id", sub, "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	if err := h.deps.Store.OpenSession(ctx, sub); err != nil {
		// PIN уже установлен, но сессию открыть нечем — fail-closed:
		// владелец сделает verify, когда Redis оживёт.
		h.failClosed(c, "pin/setup: open session", err)
		return
	}
	h.deps.Log.Info("emma: pin установлен", "manager_id", sub)
	c.JSON(http.StatusOK, gin.H{"pin_set": true, "session_active": true})
}

// POST /api/emma/pin/verify {pin} — открыть сессию / 401 / 429.
func (h *PINHandler) verify(c *gin.Context) {
	sub, ok := managerID(c)
	if !ok {
		return
	}
	var req pinBody
	if err := c.ShouldBindJSON(&req); err != nil || !pinRe.MatchString(req.PIN) {
		apiError(c, http.StatusBadRequest, "pin — ровно 6 цифр", codeValidation)
		return
	}
	ctx := c.Request.Context()

	// Блокировка проверяется ДО сравнения PIN: во время блокировки даже
	// верный PIN получает 429 (критерий приёмки — иначе перебор продолжался
	// бы под блокировкой).
	fails, retryAfter, err := h.deps.Store.FailState(ctx, sub)
	if err != nil {
		h.failClosed(c, "pin/verify: fail state", err)
		return
	}
	if fails >= MaxFails {
		h.deps.Log.Warn("emma: pin verify при блокировке", "manager_id", sub)
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
			"error":       "слишком много неверных попыток, подождите",
			"code":        CodePinLocked,
			"retry_after": int(retryAfter.Seconds()),
		})
		return
	}

	if !h.checkPIN(ctx, req.PIN) {
		if err := h.deps.Store.IncrFail(ctx, sub); err != nil {
			h.failClosed(c, "pin/verify: incr fail", err)
			return
		}
		h.deps.Log.Warn("emma: pin verify — неверный PIN", "manager_id", sub)
		apiError(c, http.StatusUnauthorized, "неверный PIN", CodePinInvalid)
		return
	}

	if err := h.deps.Store.ResetFails(ctx, sub); err != nil {
		h.failClosed(c, "pin/verify: reset fails", err)
		return
	}
	if err := h.deps.Store.OpenSession(ctx, sub); err != nil {
		h.failClosed(c, "pin/verify: open session", err)
		return
	}
	h.deps.Log.Info("emma: pin verify — сессия открыта", "manager_id", sub)
	c.JSON(http.StatusOK, gin.H{"session_active": true})
}

// POST /api/emma/pin/change {old_pin, new_pin} — по старому PIN; активная
// сессия НЕ рвётся. Счётчик брутфорса общий с verify: change тоже принимает
// PIN и без счётчика был бы обходной дорожкой перебора.
func (h *PINHandler) change(c *gin.Context) {
	sub, ok := managerID(c)
	if !ok {
		return
	}
	var req changeBody
	if err := c.ShouldBindJSON(&req); err != nil ||
		!pinRe.MatchString(req.OldPIN) || !pinRe.MatchString(req.NewPIN) {
		apiError(c, http.StatusBadRequest, "old_pin и new_pin — ровно 6 цифр", codeValidation)
		return
	}
	ctx := c.Request.Context()

	fails, retryAfter, err := h.deps.Store.FailState(ctx, sub)
	if err != nil {
		h.failClosed(c, "pin/change: fail state", err)
		return
	}
	if fails >= MaxFails {
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
			"error":       "слишком много неверных попыток, подождите",
			"code":        CodePinLocked,
			"retry_after": int(retryAfter.Seconds()),
		})
		return
	}

	if !h.checkPIN(ctx, req.OldPIN) {
		if err := h.deps.Store.IncrFail(ctx, sub); err != nil {
			h.failClosed(c, "pin/change: incr fail", err)
			return
		}
		h.deps.Log.Warn("emma: pin change — неверный старый PIN", "manager_id", sub)
		apiError(c, http.StatusUnauthorized, "неверный PIN", CodePinInvalid)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPIN), bcryptCost)
	if err != nil {
		h.deps.Log.Error("emma: pin change: bcrypt", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	if err := h.deps.Settings.SetString(ctx, settings.KeyPinHash, string(hash)); err != nil {
		h.deps.Log.Error("emma: pin change: сохранение хэша", "manager_id", sub, "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	if err := h.deps.Store.ResetFails(ctx, sub); err != nil {
		h.failClosed(c, "pin/change: reset fails", err)
		return
	}
	h.deps.Log.Info("emma: pin изменён", "manager_id", sub)
	c.JSON(http.StatusOK, gin.H{"pin_set": true})
}

// DELETE /api/emma/pin/session — выход из панели.
func (h *PINHandler) closeSession(c *gin.Context) {
	sub, ok := managerID(c)
	if !ok {
		return
	}
	if err := h.deps.Store.CloseSession(c.Request.Context(), sub); err != nil {
		h.failClosed(c, "pin/session: close", err)
		return
	}
	h.deps.Log.Info("emma: pin-сессия закрыта", "manager_id", sub)
	c.JSON(http.StatusOK, gin.H{"session_active": false})
}

// checkPIN — bcrypt-сравнение. Пустой хэш («PIN не задан») даёт false —
// наружу это тот же 401 PIN_INVALID, что и неверный PIN (см. шапку файла).
func (h *PINHandler) checkPIN(ctx context.Context, pin string) bool {
	hash := h.deps.Settings.String(ctx, settings.KeyPinHash)
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pin)) == nil
}

// failClosed — единая точка 503 PIN_UNAVAILABLE (ТЗ §2.2): Redis недоступен →
// панель закрыта. Значения PIN в лог не попадают (CLAUDE.md §5).
func (h *PINHandler) failClosed(c *gin.Context, op string, err error) {
	h.deps.Log.Error("emma: redis недоступен — fail-closed", "op", op, "error", err)
	apiError(c, http.StatusServiceUnavailable, "PIN-контур временно недоступен", CodePinUnavailable)
}
