// status.go — GET /api/emma/status (EP-01 §6, ТЗ §2.3): вкладка «Статус»
// видит только факты «задан/не задан» и имя модели. Сами значения токена
// Telegram и ключа Anthropic не выводятся и через API не передаются НИКОГДА.
package emma

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// StatusInfo — снимок фактов из конфига, собирается в cmd/server один раз
// при старте (конфиг после запуска не меняется).
type StatusInfo struct {
	TelegramTokenSet bool   // telegram.bot_token задан
	AnthropicKeySet  bool   // claude.api_key задан
	Model            string // claude.model — имя, не секрет
	WebhookURLSet    bool   // telegram.webhook_url задан
}

// StatusHandler — GET /status за полной цепочкой (auth + admin + PIN).
type StatusHandler struct {
	info StatusInfo
}

func NewStatus(info StatusInfo) *StatusHandler {
	return &StatusHandler{info: info}
}

func (h *StatusHandler) Register(g gin.IRouter) {
	g.GET("/status", h.get)
}

func (h *StatusHandler) get(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"telegram_token_set": h.info.TelegramTokenSet,
		"anthropic_key_set":  h.info.AnthropicKeySet,
		"model":              h.info.Model,
		"webhook_url_set":    h.info.WebhookURLSet,
	})
}
