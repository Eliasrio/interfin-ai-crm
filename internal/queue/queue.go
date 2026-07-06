// Package queue — постановка фоновых задач в Asynq (контракт M2 → M3).
//
// Жёсткое правило CLAUDE.md §4.5 (AQ²-fix #6): Asynq НЕ дедуплицирует задачи
// сам. Каждый enqueue обязан идти с asynq.TaskID(DedupKey(...)) + asynq.Unique.
// Повторная доставка того же telegram-апдейта даёт ErrDuplicate, а не вторую
// задачу в очереди.
package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/config"
)

// TypeProcessInbound — тип задачи «обработать входящее сообщение лида».
// Обработчик появится в M3; M2 только ставит задачи.
const TypeProcessInbound = "process:inbound"

const (
	// uniqueTTL — окно уникальности задачи (SRS §6.3: asynq.Unique(1*time.Hour)).
	uniqueTTL = 1 * time.Hour
	// maxRetry — 3 попытки (SRS §6.3; backoff 2/8/32 с задаёт воркер в M3).
	maxRetry = 3
)

// ErrDuplicate — задача с таким дедуп-ключом уже стоит в очереди.
// Для вызывающего это не сбой: апдейт уже принят ранее.
var ErrDuplicate = errors.New("queue: задача уже в очереди")

// InboundPayload — полезная нагрузка process:inbound (контракт M2 наружу).
// MsgID — message_id из Telegram (стабилен между повторными доставками
// апдейта, поэтому именно он участвует в дедуп-ключе).
type InboundPayload struct {
	LeadID int64 `json:"lead_id"`
	MsgID  int   `json:"msg_id"`
}

// DedupKey — sha256(lead_id:msg_id) hex (контракт M2, SRS §6.3).
func DedupKey(leadID int64, msgID int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", leadID, msgID)))
	return hex.EncodeToString(sum[:])
}

// Enqueuer — контракт постановки задач для HTTP-слоя (в тестах — фейк).
type Enqueuer interface {
	// EnqueueInbound ставит process:inbound. Возвращает ErrDuplicate,
	// если задача с этим (leadID, msgID) уже в очереди.
	EnqueueInbound(ctx context.Context, leadID int64, msgID int) error
}

// Client — Asynq-клиент поверх Redis из конфига приложения.
type Client struct {
	c *asynq.Client
}

// NewClient создаёт клиента очереди. Redis тот же, что для всего приложения:
// одиночный (dev) или Sentinel (prod) — по конфигу (SRS §11.1).
func NewClient(cfg config.RedisConfig) *Client {
	return &Client{c: asynq.NewClient(redisConnOpt(cfg))}
}

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

func (c *Client) EnqueueInbound(ctx context.Context, leadID int64, msgID int) error {
	payload, err := json.Marshal(InboundPayload{LeadID: leadID, MsgID: msgID})
	if err != nil {
		return fmt.Errorf("queue: marshal inbound payload: %w", err)
	}

	task := asynq.NewTask(TypeProcessInbound, payload)
	_, err = c.c.EnqueueContext(ctx, task,
		asynq.TaskID(DedupKey(leadID, msgID)), // CLAUDE.md §4.5
		asynq.Unique(uniqueTTL),               // CLAUDE.md §4.5
		asynq.MaxRetry(maxRetry),              // SRS §6.3
	)
	switch {
	case errors.Is(err, asynq.ErrTaskIDConflict), errors.Is(err, asynq.ErrDuplicateTask):
		return ErrDuplicate
	case err != nil:
		return fmt.Errorf("queue: enqueue %s: %w", TypeProcessInbound, err)
	}
	return nil
}

// Close закрывает соединение с Redis.
func (c *Client) Close() error {
	if err := c.c.Close(); err != nil {
		return fmt.Errorf("queue: close: %w", err)
	}
	return nil
}
