// Package ws — WebSocket real-time push воркер → React Kanban (M9, SRS §10).
//
// Архитектура (§10.1): бизнес-логика (M5/M6) публикует события в Redis-канал
// crm:events; Hub — единственный подписчик в процессе — раздаёт их всем
// авторизованным WS-клиентам как есть (payload не переупаковывается: схема
// events.Event и есть контракт для M10).
//
// Поверх потока событий Hub шлёт два СЛУЖЕБНЫХ сообщения о режиме доставки
// (в Redis они не публикуются, их генерирует сам Hub):
//
//	{"type":"polling_mode","poll_interval_sec":5} — pub/sub оборван (§10.1):
//	    клиент переходит на опрос GET /api/leads/:id/stage раз в 5 с;
//	{"type":"live_mode"} — подписка восстановлена: клиент делает catch-up
//	    GET /api/leads?updated_since={last_event_ts} (§10.3) и возвращается
//	    к живым событиям.
//
// Протокол реконнекта (§10.3, AQ²-5): клиент хранит ts последнего события;
// после разрыва (штатно — close 4001 по истечении access-токена, §5.3) —
// POST /auth/refresh → GET /api/leads?updated_since → новый upgrade. Так
// события, потерянные fire-and-forget pub/sub'ом в окно реконнекта,
// досинхронизируются без гарантий доставки от Redis.
//
// Heartbeat (§10.2, AQ²-11) — ТОЛЬКО protocol-level ping/pong, см. client.go.
package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/metrics"
)

// Timings — интервалы heartbeat/записи. Вынесены в структуру ради тестов
// (боевые 30/60 с растянули бы тест idle-разрыва на минуты).
type Timings struct {
	PingInterval time.Duration // серверный ping (§10.2: 30 с)
	PongWait     time.Duration // read deadline, продлевается pong'ом (§10.2: 60 с)
	WriteWait    time.Duration // дедлайн записи кадра
}

// DefaultTimings — боевые значения §10.2.
var DefaultTimings = Timings{
	PingInterval: 30 * time.Second,
	PongWait:     60 * time.Second,
	WriteWait:    10 * time.Second,
}

// resubscribeDelay — пауза между попытками восстановить подписку crm:events.
const resubscribeDelay = time.Second

// Служебные сообщения режима доставки (контракт M10, см. doc пакета).
var (
	msgPollingMode = []byte(`{"type":"polling_mode","poll_interval_sec":5}`)
	msgLiveMode    = []byte(`{"type":"live_mode"}`)
)

// Hub — broadcast-центр: одна подписка на crm:events, N клиентов (§10.1).
type Hub struct {
	timings Timings
	log     *slog.Logger

	mu       sync.Mutex
	clients  map[*client]struct{}
	degraded bool // pub/sub оборван, клиенты предупреждены о polling mode
}

func NewHub(timings Timings, log *slog.Logger) *Hub {
	return &Hub{
		timings: timings,
		log:     log,
		clients: map[*client]struct{}{},
	}
}

// Run — горутина Hub'а: держит подписку на crm:events, пока жив ctx.
// Обрыв pub/sub не роняет Hub: клиентам уходит polling_mode, подписка
// восстанавливается с ретраями, после восстановления — live_mode (§10.1).
func (h *Hub) Run(ctx context.Context, rdb redis.UniversalClient) {
	defer h.closeAll()
	for {
		if ctx.Err() != nil {
			return
		}
		pubsub := rdb.Subscribe(ctx, events.Channel)
		// Receive подтверждает подписку: до подтверждения события не идут,
		// объявлять live_mode рано.
		if _, err := pubsub.Receive(ctx); err != nil {
			_ = pubsub.Close()
			if ctx.Err() != nil {
				return
			}
			h.setDegraded(true)
			h.log.Warn("ws: подписка crm:events не удалась, ретрай", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(resubscribeDelay):
			}
			continue
		}
		h.setDegraded(false)
		h.log.Info("ws: подписка crm:events активна")
		h.pump(ctx, pubsub)
		_ = pubsub.Close()
	}
}

// pump читает события до обрыва подписки или отмены ctx.
func (h *Hub) pump(ctx context.Context, pubsub *redis.PubSub) {
	for {
		msg, err := pubsub.ReceiveMessage(ctx)
		if err != nil {
			if ctx.Err() == nil {
				h.setDegraded(true)
				h.log.Warn("ws: pub/sub оборван, клиенты в polling mode", "error", err)
			}
			return
		}
		observeEventLatency([]byte(msg.Payload))
		h.broadcast([]byte(msg.Payload))
	}
}

// observeEventLatency — ws_event_latency_seconds (M11 §14): от events.Event.TS
// (момент публикации, ставится в том же процессе — часы одни) до broadcast.
// Достаём из payload только ts: полная схема события Hub'у не нужна (§10.1 —
// он раздаёт события как есть).
func observeEventLatency(payload []byte) {
	var ev struct {
		TS time.Time `json:"ts"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil || ev.TS.IsZero() {
		return // не событие crm:events или без метки — метрике нечего мерить
	}
	if lat := time.Since(ev.TS); lat >= 0 {
		metrics.WSEventLatency.Observe(lat.Seconds())
	}
}

// setDegraded переключает режим доставки и оповещает клиентов ровно один
// раз на смену состояния (ретраи подписки не спамят polling_mode).
func (h *Hub) setDegraded(v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.degraded == v {
		return
	}
	h.degraded = v
	if v {
		h.broadcastLocked(msgPollingMode)
	} else {
		h.broadcastLocked(msgLiveMode)
	}
}

func (h *Hub) broadcast(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broadcastLocked(msg)
}

func (h *Hub) broadcastLocked(msg []byte) {
	for c := range h.clients {
		select {
		case c.send <- msg:
		default:
			// Буфер полон — клиент не вычитывает. Отключаем: держать
			// broadcast ради него нельзя, пропуски он доберёт catch-up'ом
			// §10.3 при реконнекте.
			delete(h.clients, c)
			close(c.send)
		}
	}
}

// register добавляет клиента; в degraded-режиме он сразу получает
// polling_mode — событие обрыва случилось до его подключения.
func (h *Hub) register(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = struct{}{}
	if h.degraded {
		c.send <- msgPollingMode // буфер пуст: клиент только создан
	}
}

// unregister идемпотентен: клиент мог быть уже отключён broadcast'ом
// (медленное чтение) или closeAll.
func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
}

// closeAll отключает всех клиентов (shutdown): writePump каждого дошлёт
// close-кадр going away.
func (h *Hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		delete(h.clients, c)
		close(c.send)
	}
}

// serve регистрирует клиента и блокируется до разрыва соединения
// (вызывается из HTTP-хендлера — тот владеет соединением).
func (h *Hub) serve(c *client) {
	h.register(c)
	go c.writePump(h)
	c.readPump(h)
}
