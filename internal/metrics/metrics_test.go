package metrics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// AQ²-10 (уровень приложения): /metrics с чужого IP → 403, из allowlist → 200.
// Второй слой (Nginx mTLS) проверяется security-тестом ops/nginx на стенде.
func TestIPAllowlist_ForeignIPForbidden(t *testing.T) {
	guard, err := IPAllowlist([]string{"10.0.0.0/8", "127.0.0.1/32"}, Handler())
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		remoteAddr string
		wantStatus int
	}{
		{"8.8.8.8:41234", http.StatusForbidden},     // чужой публичный IP
		{"203.0.113.7:555", http.StatusForbidden},   // чужой (TEST-NET-3)
		{"172.17.0.5:9000", http.StatusForbidden},   // приватный, но НЕ из allowlist
		{"10.1.2.3:41234", http.StatusOK},           // allowlist 10/8
		{"127.0.0.1:5000", http.StatusOK},           // loopback
		{"[2001:db8::1]:443", http.StatusForbidden}, // IPv6 вне allowlist
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.RemoteAddr = tc.remoteAddr
		rec := httptest.NewRecorder()
		guard.ServeHTTP(rec, req)
		if rec.Code != tc.wantStatus {
			t.Errorf("RemoteAddr %s: статус %d, ждали %d", tc.remoteAddr, rec.Code, tc.wantStatus)
		}
		if tc.wantStatus == http.StatusForbidden {
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Errorf("RemoteAddr %s: тело 403 не JSON: %v", tc.remoteAddr, err)
				continue
			}
			if body["code"] != "ERR_METRICS_FORBIDDEN" {
				t.Errorf("RemoteAddr %s: code = %q, ждали ERR_METRICS_FORBIDDEN (CLAUDE.md §5)",
					tc.remoteAddr, body["code"])
			}
		}
	}
}

// X-Forwarded-For не обходит allowlist: заголовок подделывается любым клиентом.
func TestIPAllowlist_XForwardedForIgnored(t *testing.T) {
	guard, err := IPAllowlist([]string{"10.0.0.0/8"}, Handler())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "8.8.8.8:41234"
	req.Header.Set("X-Forwarded-For", "10.0.0.1") // спуфинг
	rec := httptest.NewRecorder()
	guard.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("спуфнутый X-Forwarded-For пробил allowlist: статус %d", rec.Code)
	}
}

// Одиночный IP без маски — валидная запись allowlist.
func TestIPAllowlist_PlainIPEntry(t *testing.T) {
	guard, err := IPAllowlist([]string{"192.0.2.10"}, Handler())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	rec := httptest.NewRecorder()
	guard.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("одиночный IP из allowlist получил %d", rec.Code)
	}
}

// Конфигурационные ошибки ловятся на старте, а не в рантайме 500-ками.
func TestIPAllowlist_ConfigErrors(t *testing.T) {
	if _, err := IPAllowlist(nil, Handler()); err == nil {
		t.Error("пустой allowlist обязан быть ошибкой старта (иначе /metrics закрыт для всех)")
	}
	if _, err := IPAllowlist([]string{"not-an-ip"}, Handler()); err == nil {
		t.Error("мусорная запись allowlist обязана быть ошибкой старта")
	}
}

// Смоук: все пять метрик §14 реально зарегистрированы и экспонируются.
func TestHandler_ExposesM11Metrics(t *testing.T) {
	// Пишем по одному наблюдению, чтобы метрики появились в выдаче.
	GinRequestDuration.WithLabelValues("POST", "/webhook/telegram", "200").Observe(0.01)
	AsynqTaskDuration.WithLabelValues("process:inbound", "ok").Observe(1.5)
	ClaudeAPIDuration.WithLabelValues("/v1/messages", "200").Observe(4.2)
	WSEventLatency.Observe(0.002)
	AsynqQueueSize.WithLabelValues("default", "dead").Set(0)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics: статус %d", rec.Code)
	}
	body := rec.Body.String()
	for _, name := range []string{
		"gin_request_duration_seconds",
		"asynq_task_duration_seconds",
		"claude_api_duration_seconds",
		"ws_event_latency_seconds",
		"asynq_queue_size",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("метрика %s не экспонируется (§14)", name)
		}
	}
}

// NewServer поднимает listener с /metrics за allowlist'ом (интеграция слоёв).
func TestNewServer_MetricsGuarded(t *testing.T) {
	srv, err := NewServer(0, []string{"127.0.0.1/32"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// httptest ходит со 127.0.0.1 — allowlist пропускает.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics со 127.0.0.1: статус %d", resp.StatusCode)
	}
}
