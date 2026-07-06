// api.go — общее для REST API менеджера (M8, SRS §4): формат ошибок §4.2
// и разбор путевых параметров. Сами ручки — leads.go / lgpd.go.
package handlers

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// Коды ошибок §4.2 (формат {"error","code"} — CLAUDE.md §5).
// 401/403 отдаёт auth-middleware M7 (ERR_TOKEN_*, ERR_FORBIDDEN).
const (
	codeValidation        = "ERR_VALIDATION"         // 400: невалидные поля
	codeInvalidTransition = "ERR_INVALID_TRANSITION" // 400: переход запрещён §3.1
	codeNotFound          = "ERR_NOT_FOUND"          // 404
	codeStageConflict     = "ERR_STAGE_CONFLICT"     // 409: state conflict
	codeRateLimited       = "ERR_RATE_LIMITED"       // 429: 100 req/min per IP
	codeInternal          = "ERR_INTERNAL"           // 500
)

// apiError — единственная точка формирования ошибок API (§4.2).
func apiError(c *gin.Context, status int, msg, code string) {
	c.AbortWithStatusJSON(status, gin.H{"error": msg, "code": code})
}

// pathID разбирает :id из пути. false — ответ 400 уже отправлен.
func pathID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		apiError(c, 400, "id в пути должен быть положительным числом", codeValidation)
		return 0, false
	}
	return id, true
}
