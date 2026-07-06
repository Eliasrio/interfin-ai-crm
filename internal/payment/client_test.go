package payment

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/interfin/interfin-ai-crm/internal/config"
)

// testClient — клиент, направленный в httptest-сервер вместо crypt.bot.
func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewClient(config.PaymentConfig{
		Gateway:      "cryptobot",
		TestnetToken: testToken,
		UseTestnet:   true,
	})
	c.baseURL = srv.URL // тесты не ходят в сеть
	return c
}

func TestCreateInvoice(t *testing.T) {
	var gotPath, gotToken string
	var gotBody map[string]interface{}

	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get(tokenHeader)
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"ok": true, "result": {
			"invoice_id": 528, "status": "active", "asset": "USDT",
			"amount": "100", "payload": "101",
			"bot_invoice_url": "https://t.me/CryptoTestnetBot?start=IV123"
		}}`))
	})

	inv, err := c.CreateInvoice(context.Background(), 101, CreateInvoiceParams{
		Asset:       "USDT",
		Amount:      decimal.NewFromInt(100),
		Description: "Оплата консультации",
	})
	if err != nil {
		t.Fatalf("CreateInvoice: %v", err)
	}

	if gotPath != "/createInvoice" {
		t.Errorf("path = %q, ожидали /createInvoice", gotPath)
	}
	if gotToken != testToken {
		t.Errorf("заголовок %s = %q, ожидали токен активной сети", tokenHeader, gotToken)
	}
	// Ключевой контракт M6: payload инвойса = lead_id, сумма — строкой.
	if gotBody["payload"] != "101" {
		t.Errorf("payload = %v, ожидали \"101\" (lead_id)", gotBody["payload"])
	}
	if gotBody["amount"] != "100" {
		t.Errorf("amount = %v (%T), API ждёт строку \"100\"", gotBody["amount"], gotBody["amount"])
	}

	if inv.InvoiceID != 528 || inv.BotInvoiceURL == "" {
		t.Errorf("инвойс разобран неверно: %+v", inv)
	}
}

func TestCreateInvoice_APIError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok": false, "error": {"code": 401, "name": "UNAUTHORIZED"}}`))
	})

	_, err := c.CreateInvoice(context.Background(), 101, CreateInvoiceParams{
		Asset:  "USDT",
		Amount: decimal.NewFromInt(100),
	})
	if err == nil {
		t.Fatal("ошибка API должна возвращаться наверх")
	}
	if !strings.Contains(err.Error(), "UNAUTHORIZED") {
		t.Errorf("ошибка должна называть код API, получили: %v", err)
	}
}
