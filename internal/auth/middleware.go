package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// claimsKey — ключ claims в gin.Context. Приватный: доступ только
// через ClaimsFrom.
const claimsKey = "auth.claims"

// Middleware валидирует Bearer JWT из Authorization (задача M7-4).
// Нет/битый токен → 401 ERR_TOKEN_INVALID, просрочен → 401 ERR_TOKEN_EXPIRED
// (AQ²-2). Claims кладутся в контекст для RequireRole и хендлеров M8.
// Формат ошибок — {"error","code"} (CLAUDE.md §5).
func Middleware(v *Verifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		const prefix = "Bearer "
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, prefix) {
			abort(c, http.StatusUnauthorized, "требуется Bearer-токен", "ERR_TOKEN_INVALID")
			return
		}
		claims, err := v.Verify(strings.TrimSpace(header[len(prefix):]))
		switch {
		case errors.Is(err, ErrTokenExpired):
			abort(c, http.StatusUnauthorized, "access token просрочен", "ERR_TOKEN_EXPIRED")
			return
		case err != nil:
			abort(c, http.StatusUnauthorized, "access token невалиден", "ERR_TOKEN_INVALID")
			return
		}
		c.Set(claimsKey, claims)
		c.Next()
	}
}

// RequireRole пропускает только перечисленные роли (§5.2), иначе 403.
// Ставится ПОСЛЕ Middleware. admin не наследует manager неявно — M8
// перечисляет роли явно: RequireRole(RoleManager, RoleAdmin).
func RequireRole(roles ...string) gin.HandlerFunc {
	allowed := make(map[string]bool, len(roles))
	for _, r := range roles {
		allowed[r] = true
	}
	return func(c *gin.Context) {
		claims, ok := ClaimsFrom(c)
		if !ok {
			// RequireRole без Middleware — ошибка сборки роутера, не клиента.
			abort(c, http.StatusUnauthorized, "требуется аутентификация", "ERR_TOKEN_INVALID")
			return
		}
		if !allowed[claims.Role] {
			abort(c, http.StatusForbidden, "недостаточно прав", "ERR_FORBIDDEN")
			return
		}
		c.Next()
	}
}

// ClaimsFrom достаёт claims, положенные Middleware (для хендлеров M8:
// manager видит только свои лиды — фильтр по ManagerID).
func ClaimsFrom(c *gin.Context) (*Claims, bool) {
	v, ok := c.Get(claimsKey)
	if !ok {
		return nil, false
	}
	claims, ok := v.(*Claims)
	return claims, ok
}

func abort(c *gin.Context, status int, msg, code string) {
	c.AbortWithStatusJSON(status, gin.H{"error": msg, "code": code})
}
