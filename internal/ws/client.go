// client.go — одно WS-соединение: read/write pump и heartbeat §10.2.
//
// AQ²-fix #11 (жёсткое правило M9): heartbeat ТОЛЬКО protocol-level —
// серверный ping раз в PingInterval, pong клиента продлевает read deadline
// на PongWait. App-level {type:"ping"} НЕ существует — убран в SRS v2.3
// как дублирование; поток данных содержит только события и сигналы режима.
package ws

import (
	"time"

	"github.com/gorilla/websocket"
)

// CloseTokenExpired — код закрытия «access-токен истёк» (§5.3): клиент
// делает POST /auth/refresh и реконнект с catch-up (§10.3).
const CloseTokenExpired = 4001

// sendBuffer — очередь исходящих на клиента. Переполнение = клиент не
// вычитывает дольше ~32 событий → hub отключает его (см. broadcastLocked).
const sendBuffer = 32

// maxClientFrame — лимит входящего кадра: клиент по протоколу ничего не
// шлёт (push-only канал), лимит защищает read pump от мусора.
const maxClientFrame = 512

type client struct {
	conn *websocket.Conn
	send chan []byte
	// expiresAt — exp access-токена: в этот момент writePump закрывает
	// соединение кодом 4001 (§5.3) — токен не должен переживать сессию.
	expiresAt time.Time
}

// readPump — единственный читатель соединения. Данных от клиента протокол
// не предусматривает; чтение нужно, чтобы обрабатывать control-кадры
// (pong, close) и замечать разрыв. Idle-соединение без pong'ов умирает
// по read deadline — это ЕДИНСТВЕННЫЙ механизм обнаружения мёртвого
// клиента (AQ²-11).
func (c *client) readPump(h *Hub) {
	defer func() {
		h.unregister(c)
		_ = c.conn.Close()
	}()
	c.conn.SetReadLimit(maxClientFrame)
	_ = c.conn.SetReadDeadline(time.Now().Add(h.timings.PongWait))
	// §10.2: pong продлевает read deadline на PongWait.
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(h.timings.PongWait))
	})
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return // разрыв или просроченный deadline — соединение мертво
		}
		// Data-кадры от клиента игнорируются: канал push-only.
	}
}

// writePump — единственный писатель соединения (гарантия gorilla/websocket:
// один конкурентный писатель). Завершение по любой причине закрывает
// соединение — readPump выходит по ошибке чтения и снимает регистрацию.
func (c *client) writePump(h *Hub) {
	ticker := time.NewTicker(h.timings.PingInterval)
	expiry := time.NewTimer(time.Until(c.expiresAt))
	defer func() {
		ticker.Stop()
		expiry.Stop()
		_ = c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				// Hub снял клиента: shutdown или слишком медленное чтение.
				_ = c.conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseGoingAway, "server closing"),
					time.Now().Add(h.timings.WriteWait))
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(h.timings.WriteWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			// §10.2: серверный protocol-level ping — единственный heartbeat.
			if err := c.conn.WriteControl(websocket.PingMessage, nil,
				time.Now().Add(h.timings.WriteWait)); err != nil {
				return
			}
		case <-expiry.C:
			// §5.3: access-токен истёк посреди сессии → close 4001,
			// клиент уходит в reconnect с catch-up (§10.3, AQ²-5).
			_ = c.conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(CloseTokenExpired, "access token просрочен"),
				time.Now().Add(h.timings.WriteWait))
			return
		}
	}
}
