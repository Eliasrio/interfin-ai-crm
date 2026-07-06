// Package metrics — Prometheus-метрики SLO (M11, SRS §14).
//
// Пять метрик из task-файла M11:
//
//	gin_request_duration_seconds  — латентность HTTP-хендлеров (в т.ч. вебхука);
//	asynq_task_duration_seconds   — длительность фоновых задач воркера;
//	claude_api_duration_seconds   — латентность вызовов Anthropic API;
//	ws_event_latency_seconds      — путь события Redis pub/sub → broadcast Hub;
//	asynq_queue_size{state}       — размер очереди по состояниям; state="dead"
//	                                (архив asynq) питает алерт DeadLetterQueueGrowing.
//
// Эндпоинт /metrics поднимается ОТДЕЛЬНЫМ листенером (monitoring.prometheus_port),
// не на боевом порту приложения, и защищён IP-allowlist'ом (AQ²-10): чужой
// IP → 403. Снаружи периметра добавляется второй слой — Nginx mTLS (ops/nginx).
package metrics

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// registry — собственный реестр вместо глобального DefaultRegisterer:
// метрики процесса подключаются явно, а тесты не ловят duplicate registration
// от чужих пакетов.
var registry = prometheus.NewRegistry()

var (
	// GinRequestDuration — латентность HTTP-запросов. path — шаблон роута
	// (c.FullPath()), не сырой URL: кардинальность ограничена таблицей роутов.
	GinRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gin_request_duration_seconds",
		Help:    "Латентность HTTP-запросов по роутам (SRS §14).",
		Buckets: prometheus.DefBuckets, // 5мс..10с — вебхук обязан жить в начале шкалы (§4.4)
	}, []string{"method", "path", "status"})

	// AsynqTaskDuration — длительность обработки фоновой задачи воркером.
	AsynqTaskDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "asynq_task_duration_seconds",
		Help: "Длительность задач Asynq по типам (SRS §14).",
		// Узкое место — Claude 3–15 с (§6.1): шкала тянется до минуты.
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 20, 30, 60},
	}, []string{"task_type", "status"})

	// ClaudeAPIDuration — латентность вызовов Anthropic (endpoint —
	// /v1/messages или /v1/messages/count_tokens; status — HTTP-код либо
	// transport_error).
	ClaudeAPIDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "claude_api_duration_seconds",
		Help:    "Латентность вызовов Anthropic API (SRS §14, §6.1: 3–15 с).",
		Buckets: []float64{0.5, 1, 2, 3, 5, 8, 13, 21, 34, 55},
	}, []string{"endpoint", "status"})

	// WSEventLatency — от events.Event.TS (момент публикации в Redis) до
	// broadcast клиентам. Публикация и Hub живут в одном процессе — часы одни.
	WSEventLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "ws_event_latency_seconds",
		Help:    "Путь события crm:events до WS broadcast (IQ-10: < 500 мс).",
		Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5},
	})

	// AsynqQueueSize — снимок очереди по состояниям (опрос Inspector'ом,
	// см. QueueStats). state="dead" — архив asynq, терминология SRS §6.3.
	AsynqQueueSize = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "asynq_queue_size",
		Help: "Задач в очереди Asynq по состояниям; state=dead — dead letter (§6.3).",
	}, []string{"queue", "state"})
)

func init() {
	registry.MustRegister(
		GinRequestDuration,
		AsynqTaskDuration,
		ClaudeAPIDuration,
		WSEventLatency,
		AsynqQueueSize,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// Handler — HTTP-хендлер /metrics поверх собственного реестра.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

// NewServer собирает listener :port с единственным роутом /metrics за
// IP-allowlist'ом (AQ²-10). Запуск/останов — забота вызывающего (cmd/server).
func NewServer(port int, allowlist []string) (*http.Server, error) {
	guard, err := IPAllowlist(allowlist, Handler())
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", guard)
	return &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}, nil
}

// IPAllowlist пропускает к next только IP из cidrs (CIDR или одиночный IP),
// остальным — 403 в формате ошибок CLAUDE.md §5. Решение принимается по
// RemoteAddr: X-Forwarded-For сознательно игнорируется — заголовок подделывает
// любой клиент, а легитимный внешний доступ проходит через Nginx mTLS.
func IPAllowlist(cidrs []string, next http.Handler) (http.Handler, error) {
	if len(cidrs) == 0 {
		return nil, fmt.Errorf("metrics: пустой metrics_ip_allowlist — /metrics был бы закрыт для всех (AQ²-10)")
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			// Одиночный IP без маски тоже допустим.
			ip := net.ParseIP(c)
			if ip == nil {
				return nil, fmt.Errorf("metrics: allowlist %q не CIDR и не IP", c)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			n = &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}
		}
		nets = append(nets, n)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr // RemoteAddr без порта (нестандартный listener)
		}
		ip := net.ParseIP(host)
		allowed := false
		if ip != nil {
			for _, n := range nets {
				if n.Contains(ip) {
					allowed = true
					break
				}
			}
		}
		if !allowed {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "metrics access denied",
				"code":  "ERR_METRICS_FORBIDDEN",
			})
			return
		}
		next.ServeHTTP(w, r)
	}), nil
}
