package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// refreshCookieName / refreshCookiePath — refresh-токен ездит ТОЛЬКО в
// HttpOnly cookie (§5.1) и только на /auth/* (Path-scope: остальным
// эндпоинтам он не нужен и в их запросах не светится).
const (
	refreshCookieName = "refresh_token"
	refreshCookiePath = "/auth"
)

// AuthDeps — зависимости POST /auth/login и POST /auth/refresh (M7).
type AuthDeps struct {
	Managers   repo.ManagerRepo
	Tokens     repo.RefreshTokenRepo
	Issuer     *auth.Issuer
	RefreshTTL time.Duration // 7 дней (§5.1), из config auth.refresh_token_ttl
	Log        *slog.Logger
}

// AuthHandler — выдача и продление access-токенов (SRS §5.1).
type AuthHandler struct {
	deps AuthDeps
}

func NewAuth(deps AuthDeps) *AuthHandler {
	return &AuthHandler{deps: deps}
}

// Register вешает роуты аутентификации. Auth-middleware здесь НЕ ставится:
// login/refresh — единственные публичные ручки, обе авторизуются сами
// (паролем и refresh-cookie соответственно).
func (h *AuthHandler) Register(r gin.IRouter) {
	r.POST("/auth/login", h.login)
	r.POST("/auth/refresh", h.refresh)
}

type loginRequest struct {
	Email    string `json:"email" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// login — задача M7-2: пароль → { access_token, expires_in } + refresh cookie.
// Неизвестный email и неверный пароль неразличимы для клиента (единый 401),
// время ответа выровнено bcrypt-заглушкой (timing-атака перебора email).
func (h *AuthHandler) login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "требуются email и password", "code": "ERR_BAD_REQUEST"})
		return
	}

	m, err := h.deps.Managers.GetByEmail(c.Request.Context(), req.Email)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		auth.EqualizeUnknownUser(req.Password)
		h.unauthorized(c, "неверные учётные данные", "ERR_INVALID_CREDENTIALS")
		return
	case err != nil:
		h.internalError(c, "login: get manager", err)
		return
	}

	if !auth.CheckPassword(m.PasswordHash, req.Password) {
		h.unauthorized(c, "неверные учётные данные", "ERR_INVALID_CREDENTIALS")
		return
	}

	// Гигиена: просроченные refresh-строки чистятся по случаю логина
	// (отдельного cron в M7 нет). Ошибка не мешает входу.
	if err := h.deps.Tokens.DeleteExpired(c.Request.Context()); err != nil {
		h.deps.Log.Warn("auth: очистка просроченных refresh-токенов", "error", err)
	}

	h.issueTokens(c, m)
}

// refresh — задача M7-3: продление сессии ПО REFRESH-COOKIE, Authorization
// здесь не читается вовсе — иначе refresh был бы бессмыслен при истёкшем
// access-токене. Ротация: предъявленный токен гасится (repo.Consume), выдаётся
// новая пара access+refresh; повторное предъявление старого — 401.
func (h *AuthHandler) refresh(c *gin.Context) {
	cookie, err := c.Cookie(refreshCookieName)
	if err != nil || cookie == "" {
		h.unauthorized(c, "требуется refresh-токен", "ERR_REFRESH_INVALID")
		return
	}

	rt, err := h.deps.Tokens.Consume(c.Request.Context(), auth.HashRefreshToken(cookie))
	switch {
	case errors.Is(err, repo.ErrNotFound):
		h.clearRefreshCookie(c)
		h.unauthorized(c, "refresh-токен невалиден", "ERR_REFRESH_INVALID")
		return
	case err != nil:
		h.internalError(c, "refresh: consume token", err)
		return
	}

	if time.Now().After(rt.ExpiresAt) {
		// Строка уже погашена Consume — просроченный токен исчезает из БД.
		h.clearRefreshCookie(c)
		h.unauthorized(c, "refresh-токен просрочен", "ERR_REFRESH_EXPIRED")
		return
	}

	m, err := h.deps.Managers.GetByID(c.Request.Context(), rt.ManagerID)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		// Менеджер удалён/деактивирован после выдачи токена — сессия гаснет.
		h.clearRefreshCookie(c)
		h.unauthorized(c, "учётная запись недоступна", "ERR_REFRESH_INVALID")
		return
	case err != nil:
		h.internalError(c, "refresh: get manager", err)
		return
	}

	h.issueTokens(c, m)
}

// issueTokens выпускает access-токен и НОВЫЙ refresh (ротация), пишет
// refresh-cookie и отвечает { access_token, expires_in } (задача M7-2).
func (h *AuthHandler) issueTokens(c *gin.Context, m *models.Manager) {
	access, err := h.deps.Issuer.Issue(strconv.FormatInt(m.ID, 10), m.Role)
	if err != nil {
		h.internalError(c, "issue access token", err)
		return
	}

	refresh := auth.NewRefreshToken()
	err = h.deps.Tokens.Create(c.Request.Context(), &models.RefreshToken{
		TokenHash: auth.HashRefreshToken(refresh),
		ManagerID: m.ID,
		ExpiresAt: time.Now().Add(h.deps.RefreshTTL),
	})
	if err != nil {
		h.internalError(c, "store refresh token", err)
		return
	}

	h.setRefreshCookie(c, refresh, int(h.deps.RefreshTTL.Seconds()))
	c.JSON(http.StatusOK, gin.H{
		"access_token": access,
		"expires_in":   int(h.deps.Issuer.TTL().Seconds()), // 900 (§5.1)
	})
}

// setRefreshCookie — HttpOnly (§5.1) + Secure + SameSite=Strict; Path
// ограничивает cookie ручками /auth/*. Secure на localhost не мешает:
// браузеры считают localhost secure context.
func (h *AuthHandler) setRefreshCookie(c *gin.Context, token string, maxAge int) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(refreshCookieName, token, maxAge, refreshCookiePath, "", true, true)
}

func (h *AuthHandler) clearRefreshCookie(c *gin.Context) {
	h.setRefreshCookie(c, "", -1)
}

func (h *AuthHandler) unauthorized(c *gin.Context, msg, code string) {
	c.JSON(http.StatusUnauthorized, gin.H{"error": msg, "code": code})
}

func (h *AuthHandler) internalError(c *gin.Context, what string, err error) {
	// Причина — в лог, клиенту без деталей (секреты/SQL в ответы не текут).
	h.deps.Log.Error("auth: "+what, "error", err)
	c.JSON(http.StatusInternalServerError, gin.H{
		"error": "внутренняя ошибка", "code": "ERR_INTERNAL"})
}
