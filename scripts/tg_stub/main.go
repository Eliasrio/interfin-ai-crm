// tg_stub — фейковый Telegram Bot API для локального e2e M12.
//
// Боевой бот не может доставить сообщение фейковому лиду e2e-сетапа
// (chat not found), поэтому сервер на e2e-прогоне указывает на этот стаб:
// TELEGRAM_API_URL=http://localhost:8091 (config telegram.api_url; пусто в
// prod/dev = настоящий api.telegram.org). Никакой прод-логики здесь нет.
//
// Поведение:
//   - POST /bot<token>/<method> — {"ok":true,...} на любой метод telebot
//     (getMe, setWebhook, sendChatAction, ...); sendMessage запоминается;
//   - GET  /sent?chat_id=<id>   — присланные sendMessage (проверка e2e
//     «сообщение дошло лиду в Telegram»);
//   - DELETE /sent              — очистка между сценариями.
//
// Запуск: go run ./scripts/tg_stub [-addr :8091]
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sentMessage — одна доставка sendMessage: то, что проверяет e2e.
type sentMessage struct {
	ChatID int64  `json:"chat_id"`
	Text   string `json:"text"`
}

type stub struct {
	mu   sync.Mutex
	sent []sentMessage
	seq  int
}

func main() {
	addr := flag.String("addr", ":8091", "адрес прослушивания стаба")
	flag.Parse()

	s := &stub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/sent", s.handleSent)
	mux.HandleFunc("/", s.handleBotAPI)

	log.Printf("tg_stub: слушаю %s (TELEGRAM_API_URL=http://localhost%s)", *addr, *addr)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// handleBotAPI — /bot<token>/<method>: ok:true на всё, sendMessage пишется.
func (s *stub) handleBotAPI(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "bot") {
		http.NotFound(w, r)
		return
	}
	method := parts[1]

	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body) // методы без тела — норма

	switch method {
	case "getMe":
		reply(w, map[string]any{
			"id": 42, "is_bot": true, "first_name": "Interfin Stub", "username": "interfin_stub_bot",
		})
	case "sendMessage":
		chatID := asInt64(body["chat_id"])
		text, _ := body["text"].(string)
		s.mu.Lock()
		s.seq++
		seq := s.seq
		s.sent = append(s.sent, sentMessage{ChatID: chatID, Text: text})
		s.mu.Unlock()
		log.Printf("tg_stub: sendMessage chat=%d len=%d", chatID, len(text))
		reply(w, map[string]any{
			"message_id": seq,
			"date":       time.Now().Unix(),
			"chat":       map[string]any{"id": chatID, "type": "private"},
			"text":       text,
		})
	default:
		// setWebhook, deleteWebhook, sendChatAction, ...
		reply(w, true)
	}
}

// handleSent — GET: список доставок (?chat_id= фильтр), DELETE: очистка.
func (s *stub) handleSent(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodDelete:
		s.sent = nil
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		out := make([]sentMessage, 0, len(s.sent))
		var chatID int64
		if raw := r.URL.Query().Get("chat_id"); raw != "" {
			chatID, _ = strconv.ParseInt(raw, 10, 64)
		}
		for _, m := range s.sent {
			if chatID == 0 || m.ChatID == chatID {
				out = append(out, m)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	default:
		http.Error(w, "GET или DELETE", http.StatusMethodNotAllowed)
	}
}

func reply(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

// asInt64 — chat_id из JSON: telebot шлёт строкой, число тоже принимаем.
func asInt64(v any) int64 {
	switch x := v.(type) {
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case float64:
		return int64(x)
	default:
		return 0
	}
}
