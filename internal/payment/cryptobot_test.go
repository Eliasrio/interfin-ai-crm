package payment

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/interfin/interfin-ai-crm/internal/config"
)

const testToken = "12345:AAtestTokenForHMAC"

// sampleUpdate — реальный формат вебхука Crypto Pay: суммы строками,
// fee_amount числом, request_date в ISO-8601.
const sampleUpdate = `{
	"update_id": 42,
	"update_type": "invoice_paid",
	"request_date": "2026-07-06T12:00:00.000Z",
	"payload": {
		"invoice_id": 528,
		"status": "paid",
		"asset": "USDT",
		"amount": "100",
		"fee_asset": "USDT",
		"fee_amount": 1,
		"payload": "101",
		"paid_at": "2026-07-06T11:59:58.000Z",
		"bot_invoice_url": "https://t.me/CryptoTestnetBot?start=IVDCiXFbCJXl"
	}
}`

// --- §5.5: подпись HMAC-SHA256 ключом SHA256(token) ---

func TestVerifySignature_Valid(t *testing.T) {
	body := []byte(sampleUpdate)
	if !VerifySignature(testToken, body, Sign(testToken, body)) {
		t.Error("валидная подпись должна проходить проверку")
	}
}

func TestVerifySignature_Invalid(t *testing.T) {
	body := []byte(sampleUpdate)
	cases := map[string]struct {
		token, sig string
	}{
		"чужой токен":        {testToken, Sign("999:otherToken", body)},
		"мусор вместо hex":   {testToken, "не-hex-строка"},
		"пустая подпись":     {testToken, ""},
		"пустой токен":       {"", Sign(testToken, body)}, // misconfiguration ≠ пускать всех
		"подпись иного тела": {testToken, Sign(testToken, []byte(`{"update_id":43}`))},
	}
	for name, tc := range cases {
		if VerifySignature(tc.token, body, tc.sig) {
			t.Errorf("%s: подпись не должна проходить", name)
		}
	}
}

// --- разбор вебхука ---

func TestParseUpdate(t *testing.T) {
	upd, err := ParseUpdate([]byte(sampleUpdate))
	if err != nil {
		t.Fatalf("ParseUpdate: %v", err)
	}
	if upd.UpdateID != 42 || upd.UpdateType != UpdateInvoicePaid {
		t.Errorf("шапка апдейта разобрана неверно: %+v", upd)
	}
	want := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	if !upd.RequestDate.Equal(want) {
		t.Errorf("request_date = %v, ожидали %v", upd.RequestDate, want)
	}
	inv := upd.Payload
	if inv.InvoiceID != 528 || inv.Status != "paid" || inv.Asset != "USDT" {
		t.Errorf("инвойс разобран неверно: %+v", inv)
	}
	if !inv.Amount.Equal(decimal.NewFromInt(100)) {
		t.Errorf("amount = %s, ожидали 100 (строковая сумма API)", inv.Amount)
	}
	if !inv.FeeAmount.Valid || !inv.FeeAmount.Decimal.Equal(decimal.NewFromInt(1)) {
		t.Errorf("fee_amount = %+v, ожидали 1 (числовая сумма API)", inv.FeeAmount)
	}
	if inv.Payload != "101" {
		t.Errorf("payload = %q, ожидали lead_id \"101\"", inv.Payload)
	}
}

func TestParseUpdate_Garbage(t *testing.T) {
	if _, err := ParseUpdate([]byte(`{not json`)); err == nil {
		t.Error("мусорное тело должно давать ошибку разбора")
	}
}

// --- §3.3: суммы инвойса ---

func TestInvoice_NetReceived_SubtractsFee(t *testing.T) {
	upd, _ := ParseUpdate([]byte(sampleUpdate))
	inv := upd.Payload
	// Крипто-инвойс: received = amount (paid_amount пуст), net = 100 − 1.
	if !inv.AmountReceived().Equal(decimal.NewFromInt(100)) {
		t.Errorf("AmountReceived = %s, ожидали 100", inv.AmountReceived())
	}
	if !inv.NetReceived().Equal(decimal.NewFromInt(99)) {
		t.Errorf("NetReceived = %s, ожидали 99 (комиссия шлюза вычтена)", inv.NetReceived())
	}
	if inv.Currency() != "USDT" {
		t.Errorf("Currency = %q, ожидали USDT", inv.Currency())
	}
}

func TestInvoice_NetReceived_ForeignFeeAssetNotSubtracted(t *testing.T) {
	inv := Invoice{
		Asset:     "USDT",
		Amount:    decimal.NewFromInt(100),
		FeeAsset:  "TON", // комиссия в другом активе — вычитать нечестно
		FeeAmount: decimal.NullDecimal{Decimal: decimal.NewFromInt(1), Valid: true},
	}
	if !inv.NetReceived().Equal(decimal.NewFromInt(100)) {
		t.Errorf("NetReceived = %s: комиссия в чужом активе не должна вычитаться", inv.NetReceived())
	}
}

// --- контракт invoice ↔ лид ---

func TestLeadPayloadRoundtrip(t *testing.T) {
	id, err := LeadIDFromPayload(LeadPayload(12345))
	if err != nil || id != 12345 {
		t.Errorf("roundtrip payload: id=%d err=%v", id, err)
	}
}

func TestLeadIDFromPayload_Invalid(t *testing.T) {
	for _, bad := range []string{"", "abc", "-5", "0"} {
		if _, err := LeadIDFromPayload(bad); err == nil {
			t.Errorf("payload %q должен отвергаться", bad)
		}
	}
}

// --- выбор сети testnet/mainnet ---

func TestNetworkSwitch(t *testing.T) {
	cfg := config.PaymentConfig{
		Gateway:      "cryptobot",
		TestnetToken: "111:testnet",
		MainnetToken: "222:mainnet",
	}

	cfg.UseTestnet = true
	if cfg.ActiveToken() != "111:testnet" || BaseURL(cfg) != testnetAPIBase {
		t.Errorf("use_testnet=true: token=%q url=%q", cfg.ActiveToken(), BaseURL(cfg))
	}

	cfg.UseTestnet = false
	if cfg.ActiveToken() != "222:mainnet" || BaseURL(cfg) != mainnetAPIBase {
		t.Errorf("use_testnet=false: token=%q url=%q", cfg.ActiveToken(), BaseURL(cfg))
	}
}
