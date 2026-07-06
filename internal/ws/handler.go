// handler.go — GET /ws/kanban: upgrade с JWT-аутентификацией (§5.3).
//
// Браузерный WebSocket API не умеет ставить Authorization, поэтому токен
// едет в Sec-WebSocket-Protocol: Bearer.<token> (helper M7). Сервер обязан
// эхом вернуть согласованный субпротокол — иначе браузер сам разорвёт
// соединение после handshake.
package ws

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/interfin/interfin-ai-crm/internal/auth"
)

var upgrader = websocket.Upgrader{
	// Origin не проверяется сознательно: auth — JWT в субпротоколе, не
	// cookie, так что cross-site WebSocket hijacking через браузерные
	// куки невозможен, а дефолтная same-host проверка ломала бы законные
	// подключения React (dev-сервер и API живут на разных origin).
	CheckOrigin: func(*http.Request) bool { return true },
}

// Handler — HTTP-слой WS: аутентификация при upgrade и передача
// соединения Hub'у.
type Handler struct {
	hub      *Hub
	verifier *auth.Verifier
	log      *slog.Logger
}

func NewHandler(hub *Hub, verifier *auth.Verifier, log *slog.Logger) *Handler {
	return &Handler{hub: hub, verifier: verifier, log: log}
}

// Register вешает GET /ws/kanban на корневой роутер: WS живёт вне группы
// /api (auth здесь свой — из субпротокола, а не Authorization-заголовка).
func (h *Handler) Register(r gin.IRouter) {
	r.GET("/ws/kanban", h.upgrade)
}

func (h *Handler) upgrade(c *gin.Context) {
	claims, proto, err := h.verifier.VerifyWSProtocol(
		c.GetHeader("Sec-WebSocket-Protocol"))
	switch {
	case errors.Is(err, auth.ErrTokenExpired):
		// §5.3: истёкший токен → close 4001. Код закрытия доедет до
		// браузера только после успешного handshake (статус и тело
		// неудавшегося upgrade JS-клиенту недоступны), поэтому сначала
		// завершаем upgrade с эхом субпротокола, затем закрываем.
		conn, uerr := upgrader.Upgrade(c.Writer, c.Request, protoHeader(proto))
		if uerr != nil {
			return // upgrader уже ответил клиенту сам
		}
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(CloseTokenExpired, "access token просрочен"),
			time.Now().Add(h.hub.timings.WriteWait))
		_ = conn.Close()
		return
	case err != nil:
		// Токена нет или он битый — это не reconnect-сценарий §10.3,
		// обычный 401 в формате API (CLAUDE.md §5).
		c.AbortWithStatusJSON(http.StatusUnauthorized,
			gin.H{"error": "access token невалиден", "code": "ERR_TOKEN_INVALID"})
		return
	}
	// §5.2: доска — для manager и admin (перечислены явно, admin не
	// наследует manager); system — только internal, WS ему не положен.
	if claims.Role != auth.RoleManager && claims.Role != auth.RoleAdmin {
		c.AbortWithStatusJSON(http.StatusForbidden,
			gin.H{"error": "недостаточно прав", "code": "ERR_FORBIDDEN"})
		return
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, protoHeader(proto))
	if err != nil {
		// Upgrade сам пишет ответ об ошибке; нам остаётся лог.
		h.log.Warn("ws: upgrade не удался", "error", err)
		return
	}
	h.log.Info("ws: клиент подключён", "manager", claims.Subject, "role", claims.Role)
	h.hub.serve(&client{
		conn:      conn,
		send:      make(chan []byte, sendBuffer),
		expiresAt: claims.ExpiresAt.Time,
	})
}

// protoHeader — эхо согласованного субпротокола в ответе handshake (§5.3).
func protoHeader(proto string) http.Header {
	if proto == "" {
		return nil
	}
	return http.Header{"Sec-WebSocket-Protocol": []string{proto}}
}
