package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/interfin/interfin-ai-crm/internal/config"
)

func testConfig() config.ClaudeConfig {
	return config.ClaudeConfig{
		APIKey:            "test-key",
		Model:             "claude-sonnet-5",
		ClaudeReplyTokens: 1000,
	}
}

// newTestClient — клиент, направленный в httptest-сервер вместо Anthropic.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	c, err := New(testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.baseURL = ts.URL
	return c
}

func TestNew_Validation(t *testing.T) {
	for name, mutate := range map[string]func(*config.ClaudeConfig){
		"пустой api_key":       func(c *config.ClaudeConfig) { c.APIKey = "" },
		"пустая model":         func(c *config.ClaudeConfig) { c.Model = "" },
		"нулевой reply_tokens": func(c *config.ClaudeConfig) { c.ClaudeReplyTokens = 0 },
	} {
		cfg := testConfig()
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New должен вернуть ошибку", name)
		}
	}
}

func TestComplete_RequestShapeAndResponse(t *testing.T) {
	var gotPath, gotKey, gotVersion string
	var gotBody map[string]interface{}

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-Api-Key")
		gotVersion = r.Header.Get("Anthropic-Version")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		// Ответ с нетекстовым блоком: клиент обязан склеить только text.
		w.Write([]byte(`{"content":[
			{"type":"text","text":"Здравствуйте! "},
			{"type":"tool_use","id":"x"},
			{"type":"text","text":"Чем помочь?"}
		],"usage":{"input_tokens":321,"output_tokens":45}}`))
	})

	reply, usage, err := c.Complete(context.Background(), "system prompt", []Message{
		{Role: RoleUser, Content: "привет"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if reply != "Здравствуйте! Чем помочь?" {
		t.Errorf("reply = %q", reply)
	}
	// EP-06: usage — источник учёта расходов вкладки 6.
	if usage.InputTokens != 321 || usage.OutputTokens != 45 {
		t.Errorf("usage = %+v, ожидали 321/45", usage)
	}

	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, ожидали /v1/messages", gotPath)
	}
	if gotKey != "test-key" || gotVersion != apiVersion {
		t.Errorf("заголовки: x-api-key=%q, anthropic-version=%q", gotKey, gotVersion)
	}
	if gotBody["model"] != "claude-sonnet-5" {
		t.Errorf("model = %v", gotBody["model"])
	}
	// §7.2: claude_reply_tokens → max_tokens.
	if gotBody["max_tokens"] != float64(1000) {
		t.Errorf("max_tokens = %v, ожидали 1000", gotBody["max_tokens"])
	}
	if gotBody["system"] != "system prompt" {
		t.Errorf("system = %v", gotBody["system"])
	}
	// Thinking обязан быть выключен явно: adaptive-режим по умолчанию
	// съедал бы claude_reply_tokens (бюджет §7.2).
	thinking, ok := gotBody["thinking"].(map[string]interface{})
	if !ok || thinking["type"] != "disabled" {
		t.Errorf("thinking = %v, ожидали {type: disabled}", gotBody["thinking"])
	}
}

func TestComplete_APIError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit hit"}}`))
	})

	_, _, err := c.Complete(context.Background(), "", []Message{{Role: RoleUser, Content: "hi"}})
	if err == nil {
		t.Fatal("ожидали ошибку API")
	}
	if !strings.Contains(err.Error(), "rate_limit_error") || !strings.Contains(err.Error(), "limit hit") {
		t.Errorf("ошибка должна нести тип и сообщение API, получили: %v", err)
	}
}

func TestComplete_EmptyMessages(t *testing.T) {
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) {
		t.Error("запрос не должен уходить при пустом списке сообщений")
	})
	if _, _, err := c.Complete(context.Background(), "sys", nil); err == nil {
		t.Fatal("ожидали ошибку про пустые messages")
	}
}

func TestCountTokens(t *testing.T) {
	var gotPath string
	var gotBody map[string]interface{}

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Write([]byte(`{"input_tokens": 7777}`))
	})

	n, err := c.CountTokens(context.Background(), "sys", []Message{{Role: RoleUser, Content: "тест"}})
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if n != 7777 {
		t.Errorf("input_tokens = %d, ожидали 7777", n)
	}
	if gotPath != "/v1/messages/count_tokens" {
		t.Errorf("path = %q", gotPath)
	}
	// count_tokens не принимает max_tokens — поле не должно сериализоваться.
	if _, ok := gotBody["max_tokens"]; ok {
		t.Error("max_tokens не должен уходить в count_tokens")
	}
}
