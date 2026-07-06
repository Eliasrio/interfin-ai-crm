// server.go — Asynq-сервер воркера (SRS §6.3: ретраи, dead letter, алерт).
//
// Ретраи: 3 попытки с backoff 2/8/32 с (MaxRetry=3 задаёт enqueue в M2,
// задержки — RetryDelayFunc здесь). Исчерпал ретраи → Asynq архивирует
// задачу (dead letter, в терминах SRS — asynq:dead) → алерт менеджеру
// в Telegram + структурный лог.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/queue"
)

// retryDelays — backoff SRS §6.3: 2 с после 1-й неудачи, 8 после 2-й, 32 после 3-й.
var retryDelays = []time.Duration{2 * time.Second, 8 * time.Second, 32 * time.Second}

// RetryDelay — RetryDelayFunc для asynq: n — номер завершившейся неудачей
// попытки (1..MaxRetry+1). Выход за таблицу упирается в последнее значение.
func RetryDelay(n int, _ error, _ *asynq.Task) time.Duration {
	if n < 1 {
		n = 1
	}
	if n > len(retryDelays) {
		n = len(retryDelays)
	}
	return retryDelays[n-1]
}

// concurrency — воркеров на процесс. Узкое место пайплайна — latency Claude
// (3–15 с), 10 параллельных обработчиков достаточно для нагрузки SRS §1.
const concurrency = 10

// Server — Asynq-сервер с зарегистрированными обработчиками M3.
type Server struct {
	srv *asynq.Server
	mux *asynq.ServeMux
	log *slog.Logger
}

// Option — точечная настройка сервера (нужна тестам: боевой backoff 2/8/32 с
// растянул бы стресс-тест дедупа и dead letter на минуты).
type Option func(*asynq.Config)

// WithRetryDelayFunc подменяет backoff-функцию (только для тестов).
func WithRetryDelayFunc(f asynq.RetryDelayFunc) Option {
	return func(c *asynq.Config) { c.RetryDelayFunc = f }
}

// New собирает воркер: очередь та же, что у клиента M2 (queue.redisConnOpt).
// managerChatID — Telegram-чат менеджера для алертов dead letter
// (0 = алерты только в лог; TELEGRAM_MANAGER_CHAT_ID, §12).
func New(
	redisCfg config.RedisConfig,
	proc *Processor,
	summarizer *Summarizer,
	alertSender Sender,
	managerChatID int64,
	log *slog.Logger,
	opts ...Option,
) *Server {
	cfg := asynq.Config{
		Concurrency:    concurrency,
		RetryDelayFunc: RetryDelay,
		ErrorHandler:   deadLetterAlert(alertSender, managerChatID, log),
		Logger:         asynqSlog{log: log.With("component", "asynq")},
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	mux := asynq.NewServeMux()
	mux.HandleFunc(queue.TypeProcessInbound, proc.HandleProcessInbound)
	// M4 §7.3: фоновая генерация сводки диалога (гонку гасит Redis-замок).
	mux.HandleFunc(queue.TypeSummaryGenerate, summarizer.HandleSummaryGenerate)
	// Обработчики state machine (ttl:expire, antispam:*) подключает
	// RegisterKanban до Start — боевая сборка cmd/server обязана его звать.

	return &Server{
		srv: asynq.NewServer(redisConnOpt(redisCfg), cfg),
		mux: mux,
		log: log,
	}
}

// redisConnOpt — та же логика выбора single/Sentinel, что в queue.NewClient.
func redisConnOpt(cfg config.RedisConfig) asynq.RedisConnOpt {
	if len(cfg.SentinelAddrs) > 0 {
		return asynq.RedisFailoverClientOpt{
			MasterName:    cfg.MasterName,
			SentinelAddrs: cfg.SentinelAddrs,
			Password:      cfg.Password,
		}
	}
	return asynq.RedisClientOpt{Addr: cfg.Addr, Password: cfg.Password}
}

// RegisterKanban подключает обработчики M5: ttl:expire (§3.4, тип задачи —
// контракт M3) и antispam:followup / antispam:escalate (§3.5, AQ²-fix #8).
// Зовётся до Start.
func (s *Server) RegisterKanban(h *KanbanHandlers) {
	s.mux.HandleFunc(queue.TypeTTLExpire, h.HandleTTLExpire)
	s.mux.HandleFunc(queue.TypeAntiSpamFollowup, h.HandleAntiSpamFollowup)
	s.mux.HandleFunc(queue.TypeAntiSpamEscalate, h.HandleAntiSpamEscalate)
}

// RegisterLGPD подключает обработчик lgpd:retention (§9.1, M8).
// Зовётся до Start.
func (s *Server) RegisterLGPD(h *LGPDHandlers) {
	s.mux.HandleFunc(queue.TypeLGPDRetention, h.HandleLGPDRetention)
}

// Start запускает воркер (неблокирующе — asynq.Server.Start).
func (s *Server) Start() error {
	if err := s.srv.Start(s.mux); err != nil {
		return fmt.Errorf("worker: start: %w", err)
	}
	return nil
}

// Shutdown ждёт завершения активных задач и гасит воркер (graceful).
func (s *Server) Shutdown() {
	s.srv.Shutdown()
}

// deadLetterAlert — ErrorHandler Asynq: зовётся на КАЖДУЮ неудачную попытку.
// Промежуточные — warn в лог; финальная (ретраи исчерпаны или SkipRetry) —
// error в лог + алерт менеджеру в Telegram (§6.3).
func deadLetterAlert(sender Sender, managerChatID int64, log *slog.Logger) asynq.ErrorHandlerFunc {
	return func(ctx context.Context, task *asynq.Task, err error) {
		retried, _ := asynq.GetRetryCount(ctx)
		maxRetry, _ := asynq.GetMaxRetry(ctx)
		taskID, _ := asynq.GetTaskID(ctx)

		final := retried >= maxRetry || errors.Is(err, asynq.SkipRetry)
		if !final {
			log.Warn("worker: задача упала, будет ретрай",
				"task_type", task.Type(), "task_id", taskID,
				"retried", retried, "max_retry", maxRetry, "error", err)
			return
		}

		log.Error("worker: задача в dead letter (архив asynq)",
			"task_type", task.Type(), "task_id", taskID,
			"retried", retried, "error", err)

		if managerChatID == 0 {
			return // чат менеджера не сконфигурирован — остаёмся при логе
		}
		alert := fmt.Sprintf(
			"⚠️ CRM: задача %s (id %s) не выполнена после %d попыток и ушла в dead letter.\nОшибка: %v\nPayload: %s",
			task.Type(), taskID, retried+1, err, task.Payload())
		if serr := sender.Send(managerChatID, alert); serr != nil {
			log.Error("worker: алерт менеджеру не доставлен",
				"task_id", taskID, "error", serr)
		}
	}
}

// asynqSlog — адаптер внутреннего логгера asynq на slog (единый JSON-лог §5).
type asynqSlog struct{ log *slog.Logger }

func (l asynqSlog) Debug(args ...interface{}) { l.log.Debug(fmt.Sprint(args...)) }
func (l asynqSlog) Info(args ...interface{})  { l.log.Info(fmt.Sprint(args...)) }
func (l asynqSlog) Warn(args ...interface{})  { l.log.Warn(fmt.Sprint(args...)) }
func (l asynqSlog) Error(args ...interface{}) { l.log.Error(fmt.Sprint(args...)) }
func (l asynqSlog) Fatal(args ...interface{}) { l.log.Error("FATAL: " + fmt.Sprint(args...)) }
