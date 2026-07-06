// client.go — минимальный клиент Crypto Pay API: создание инвойса.
//
// Потребители появятся в M8/M10 (менеджер выставляет счёт из UI) и в диалоге
// бота; M6 определяет контракт уже сейчас, чтобы invoice.payload ГАРАНТИРОВАННО
// содержал lead_id — иначе вебхуку не с кем связать оплату.
package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/shopspring/decimal"

	"github.com/interfin/interfin-ai-crm/internal/config"
)

// tokenHeader — аутентификация Crypto Pay API.
const tokenHeader = "Crypto-Pay-API-Token"

// maxResponseBytes — предохранитель от гигантских ответов API.
const maxResponseBytes = 1 << 20

// Client — HTTP-клиент Crypto Pay API выбранной конфигом сети.
type Client struct {
	token   string
	baseURL string
	http    *http.Client
}

func NewClient(cfg config.PaymentConfig) *Client {
	return &Client{
		token:   cfg.ActiveToken(),
		baseURL: BaseURL(cfg),
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// CreateInvoiceParams — параметры createInvoice (подмножество API §3.3).
type CreateInvoiceParams struct {
	Asset       string          `json:"asset"`  // USDT | USDC (§3.3)
	Amount      decimal.Decimal `json:"amount"` // amount_due; decimal маршалится строкой, как ждёт API
	Description string          `json:"description,omitempty"`
	ExpiresIn   int             `json:"expires_in,omitempty"` // секунды
	// Payload клиент заполняет сам: LeadPayload(leadID).
	Payload string `json:"payload"`
}

// apiEnvelope — конверт всех ответов Crypto Pay: {ok, result | error}.
type apiEnvelope struct {
	Ok     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *apiError       `json:"error"`
}

type apiError struct {
	Code int    `json:"code"`
	Name string `json:"name"`
}

// CreateInvoice выставляет лиду leadID инвойс на params.Amount params.Asset
// и возвращает его (платёжная ссылка — Invoice.BotInvoiceURL).
func (c *Client) CreateInvoice(ctx context.Context, leadID int64, params CreateInvoiceParams) (*Invoice, error) {
	params.Payload = LeadPayload(leadID)
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("payment: createInvoice: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/createInvoice", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("payment: createInvoice: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(tokenHeader, c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("payment: createInvoice: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("payment: createInvoice: чтение ответа: %w", err)
	}

	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("payment: createInvoice: ответ не JSON (HTTP %d): %w", resp.StatusCode, err)
	}
	if !env.Ok {
		if env.Error != nil {
			return nil, fmt.Errorf("payment: createInvoice: crypto pay ошибка %d %s", env.Error.Code, env.Error.Name)
		}
		return nil, fmt.Errorf("payment: createInvoice: crypto pay ok=false (HTTP %d)", resp.StatusCode)
	}

	var inv Invoice
	if err := json.Unmarshal(env.Result, &inv); err != nil {
		return nil, fmt.Errorf("payment: createInvoice: разбор result: %w", err)
	}
	return &inv, nil
}
