// Package claude — минимальный HTTP-клиент Anthropic Messages API (SRS §7).
//
// Ровно два вызова, которые нужны воркеру M3:
//   - Complete    → POST /v1/messages            (ответ бота лиду);
//   - CountTokens → POST /v1/messages/count_tokens (точный подсчёт — только
//     у границы бюджета, гибридная схема §7.2 / CLAUDE.md §4.6).
//
// Официальный Go SDK не используется сознательно: зависимости проекта
// пришпилены под Go 1.22 (CLAUDE.md §3), а нужных вызовов всего два.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/config"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	// apiVersion — обязательный заголовок anthropic-version.
	apiVersion = "2023-06-01"
	// httpTimeout — верхняя граница ожидания ответа Claude. SRS §6.1: latency
	// 3–15 с; 60 с — запас на медленные ответы, дальше пусть ретраит Asynq.
	httpTimeout = 60 * time.Second
)

// Message — одна реплика диалога в формате Messages API.
// Content всегда строка: воркер M3 шлёт только текст.
type Message struct {
	Role    string `json:"role"` // user | assistant
	Content string `json:"content"`
}

// Роли Messages API. Маппинг на §8.2: inbound → user, outbound → assistant.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Client — потокобезопасный клиент; создаётся один раз на процесс.
type Client struct {
	apiKey     string
	model      string
	maxTokens  int
	baseURL    string
	httpClient *http.Client
}

// New собирает клиента из claude-секции конфига (§12).
func New(cfg config.ClaudeConfig) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("claude: пустой api_key (ANTHROPIC_API_KEY)")
	}
	if cfg.Model == "" {
		return nil, errors.New("claude: пустой model")
	}
	if cfg.ClaudeReplyTokens <= 0 {
		return nil, fmt.Errorf("claude: claude_reply_tokens = %d, ожидали > 0", cfg.ClaudeReplyTokens)
	}
	return &Client{
		apiKey:     cfg.APIKey,
		model:      cfg.Model,
		maxTokens:  cfg.ClaudeReplyTokens,
		baseURL:    defaultBaseURL,
		httpClient: &http.Client{Timeout: httpTimeout},
	}, nil
}

// messagesRequest — тело POST /v1/messages; для count_tokens то же самое
// без max_tokens (endpoint его не принимает).
type messagesRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens,omitempty"`
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
	// Thinking выключается явно: на claude-sonnet-5 без параметра thinking
	// включается adaptive-режим по умолчанию, и «размышления» съедают
	// claude_reply_tokens (max_tokens общий), ломая бюджет §7.2 и latency §6.1.
	Thinking *thinkingConfig `json:"thinking,omitempty"`
}

type thinkingConfig struct {
	Type string `json:"type"` // adaptive | disabled
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type messagesResponse struct {
	Content []contentBlock `json:"content"`
}

type countTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

// apiError — формат ошибки Anthropic: {"type":"error","error":{...}}.
type apiError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete вызывает /v1/messages и возвращает текст ответа модели
// (claude_reply_tokens задаёт max_tokens — §7.2). Ошибка не расшифровывается
// на retryable/фатальную: политика ретраев — забота Asynq (§6.3).
func (c *Client) Complete(ctx context.Context, system string, msgs []Message) (string, error) {
	if len(msgs) == 0 {
		return "", errors.New("claude: complete: пустой список сообщений")
	}
	var resp messagesResponse
	err := c.post(ctx, "/v1/messages", messagesRequest{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		System:    system,
		Messages:  msgs,
		Thinking:  &thinkingConfig{Type: "disabled"},
	}, &resp)
	if err != nil {
		return "", fmt.Errorf("claude: complete: %w", err)
	}

	var text string
	for _, block := range resp.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}
	if text == "" {
		return "", errors.New("claude: complete: в ответе нет текстовых блоков")
	}
	return text, nil
}

// CountTokens — точный подсчёт входных токенов запроса. Вызывается ТОЛЬКО
// когда локальная оценка len/4 превысила порог (CLAUDE.md §4.6): каждый
// вызов — лишний round-trip к Anthropic.
func (c *Client) CountTokens(ctx context.Context, system string, msgs []Message) (int, error) {
	if len(msgs) == 0 {
		return 0, errors.New("claude: count_tokens: пустой список сообщений")
	}
	var resp countTokensResponse
	err := c.post(ctx, "/v1/messages/count_tokens", messagesRequest{
		Model:    c.model,
		System:   system,
		Messages: msgs,
	}, &resp)
	if err != nil {
		return 0, fmt.Errorf("claude: count_tokens: %w", err)
	}
	return resp.InputTokens, nil
}

func (c *Client) post(ctx context.Context, path string, body, out interface{}) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Anthropic-Version", apiVersion)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var ae apiError
		if json.Unmarshal(raw, &ae) == nil && ae.Error.Message != "" {
			return fmt.Errorf("api %s: %s: %s", resp.Status, ae.Error.Type, ae.Error.Message)
		}
		return fmt.Errorf("api %s", resp.Status)
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}
	return nil
}
