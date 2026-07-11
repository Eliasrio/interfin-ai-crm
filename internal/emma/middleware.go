// middleware.go — PIN-middleware и аудит группы /api/emma (EP-01 §5, §8).
//
// Контракт для EP-02…EP-06: свои ручки вешаются на группу, собранную как
//
//	emma := api.Group("/emma", auth.RequireRole(auth.RoleAdmin), emma.Audit(log))
//	pin := emma.Group("/pin")            // без RequirePIN — bootstrap
//	protected := emma.Group("", emma.RequirePIN(store))
//
// — и ничего про PIN не знают.
package emma

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/auth"
)

// RequirePIN пропускает только запросы с живой PIN-сессией и продлевает
// её (sliding TTL — ТЗ §2.2). Нет сессии → 401 PIN_REQUIRED; Redis
// недоступен → 503 PIN_UNAVAILABLE (fail-closed). Ставится ПОСЛЕ
// auth.Middleware + RequireRole(admin).
func RequirePIN(store Store, log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		sub, ok := managerID(c)
		if !ok {
			return
		}
		alive, err := store.TouchSession(c.Request.Context(), sub)
		if err != nil {
			log.Error("emma: redis недоступен — fail-closed", "op", "middleware", "error", err)
			apiError(c, http.StatusServiceUnavailable,
				"PIN-контур временно недоступен", CodePinUnavailable)
			return
		}
		if !alive {
			apiError(c, http.StatusUnauthorized, "требуется PIN", CodePinRequired)
			return
		}
		c.Next()
	}
}

// Audit — slog-след каждого мутирующего запроса панели (ТЗ §2.3, task §8):
// manager_id, метод, путь, статус; timestamp даёт slog. Вешается на корень
// группы /api/emma — будущие ручки EP-02…EP-06 получают аудит бесплатно.
// Тела запросов не логируются — там могут быть PIN (CLAUDE.md §5).
func Audit(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			c.Next()
			return
		}
		c.Next()
		sub := ""
		if claims, ok := auth.ClaimsFrom(c); ok {
			sub = claims.Subject
		}
		log.Info("emma: audit",
			"manager_id", sub,
			"method", c.Request.Method,
			"path", c.FullPath(),
			"status", c.Writer.Status())
	}
}
