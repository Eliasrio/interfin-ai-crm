// Package payment — интеграция M6 с крипто-шлюзом CryptoBot (Crypto Pay API,
// https://help.crypt.bot/crypto-pay-api): типы вебхука, проверка подписи §5.5,
// replay-защита (nonce store) и клиент createInvoice.
//
// Сеть выбирается конфигом payment.use_testnet (env CRYPTOBOT_USE_TESTNET):
// true — testnet (@CryptoTestnetBot, разработка), false — mainnet (@CryptoBot,
// боевой режим). Токен приложения и base URL API переключаются ВМЕСТЕ
// (config.ActiveToken + BaseURL): подпись testnet-вебхука mainnet-токеном
// не сойдётся, а инвойс, выставленный не в той сети, не оплатится.
package payment

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/interfin/interfin-ai-crm/internal/config"
)

// Gateway — значение payment_events.gateway для этого шлюза (§8.3).
const Gateway = "cryptobot"

// SignatureHeader — заголовок, которым Crypto Pay подписывает каждый вебхук:
// hex(HMAC-SHA256(body)) ключом SHA256(токен приложения).
const SignatureHeader = "Crypto-Pay-Api-Signature"

// UpdateInvoicePaid — единственный тип апдейта текущего Crypto Pay API:
// инвойс оплачен. Прочие типы (появятся в будущих версиях) вебхук
// подтверждает 200 без обработки.
const UpdateInvoicePaid = "invoice_paid"

// Update — тело вебхука Crypto Pay.
type Update struct {
	UpdateID    int64     `json:"update_id"`    // nonce replay-защиты §5.5
	UpdateType  string    `json:"update_type"`  // invoice_paid
	RequestDate time.Time `json:"request_date"` // timestamp replay-защиты §5.5 (±5 мин)
	Payload     Invoice   `json:"payload"`
}

// Invoice — инвойс Crypto Pay (подмножество полей, нужное §3.3/§8.3).
// Суммы в API — строки; храним decimal, не float: крипто-суммы, NUMERIC(20,8).
type Invoice struct {
	InvoiceID int64  `json:"invoice_id"`
	Status    string `json:"status"` // active | paid | expired
	Asset     string `json:"asset"`  // USDT | USDC | ...
	// Amount — сумма инвойса; это amount_due §8.3.
	Amount decimal.Decimal `json:"amount"`
	// PaidAsset/PaidAmount Crypto Pay заполняет только у фиатных инвойсов;
	// у крипто-инвойсов (наш случай, §3.3) оплаченная сумма равна Amount.
	PaidAsset  string              `json:"paid_asset"`
	PaidAmount decimal.NullDecimal `json:"paid_amount"`
	// FeeAsset/FeeAmount — комиссия, удержанная шлюзом при оплате: именно она
	// делает net_received < amount_due (§3.3: gas fees на стороне шлюза,
	// оперируем net_received).
	FeeAsset  string              `json:"fee_asset"`
	FeeAmount decimal.NullDecimal `json:"fee_amount"`
	// Payload — сквозные данные, заданные при createInvoice: у нас там lead_id
	// (контракт LeadPayload/LeadIDFromPayload).
	Payload       string     `json:"payload"`
	BotInvoiceURL string     `json:"bot_invoice_url"`
	PaidAt        *time.Time `json:"paid_at"`
}

// AmountReceived — оплаченная сумма: paid_amount, у крипто-инвойсов — amount.
func (i Invoice) AmountReceived() decimal.Decimal {
	if i.PaidAmount.Valid {
		return i.PaidAmount.Decimal
	}
	return i.Amount
}

// NetReceived — получено за вычетом комиссии шлюза (§3.3). Комиссию в другом
// активе не вычитаем: смешивать валюты в одной разности нельзя.
func (i Invoice) NetReceived() decimal.Decimal {
	net := i.AmountReceived()
	if i.FeeAmount.Valid && (i.FeeAsset == "" || i.FeeAsset == i.Currency()) {
		net = net.Sub(i.FeeAmount.Decimal)
	}
	return net
}

// Currency — актив платежа для payment_events.currency.
func (i Invoice) Currency() string {
	if i.PaidAsset != "" {
		return i.PaidAsset
	}
	return i.Asset
}

// ParseUpdate разбирает тело вебхука. Зовётся ПОСЛЕ VerifySignature:
// подпись считается от сырых байт, разбор её не касается.
func ParseUpdate(body []byte) (*Update, error) {
	var upd Update
	if err := json.Unmarshal(body, &upd); err != nil {
		return nil, fmt.Errorf("payment: разбор вебхука crypto pay: %w", err)
	}
	return &upd, nil
}

// secretKey — ключ HMAC по спецификации Crypto Pay: SHA256(токен приложения).
// Требование task-файла M6 «ключ payment.hmac_secret» реализовано этим
// производным ключом: отдельный статический секрет разъезжался бы с токеном
// при переключении testnet/mainnet (см. config/config.yaml, секция payment).
func secretKey(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Sign — hex(HMAC-SHA256(body)) ключом secretKey(token). Экспортирована для
// тестов и локальной эмуляции шлюза (curl -H "Crypto-Pay-Api-Signature: ...").
func Sign(token string, body []byte) string {
	mac := hmac.New(sha256.New, secretKey(token))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature — §5.5: подпись тела сходится с заголовком
// Crypto-Pay-Api-Signature. Пустой токен = misconfiguration → не пускаем
// никого (та же логика, что у телеграмного SecretToken, M2).
func VerifySignature(token string, body []byte, signatureHex string) bool {
	if token == "" || signatureHex == "" {
		return false
	}
	got, err := hex.DecodeString(signatureHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secretKey(token))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), got)
}

// LeadPayload / LeadIDFromPayload — контракт связи «инвойс ↔ лид»: при
// createInvoice в payload кладётся lead_id, вебхук достаёт его обратно.
func LeadPayload(leadID int64) string { return strconv.FormatInt(leadID, 10) }

func LeadIDFromPayload(payload string) (int64, error) {
	id, err := strconv.ParseInt(payload, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("payment: payload инвойса не содержит lead_id: %q", payload)
	}
	return id, nil
}

// Base URL Crypto Pay API по сетям.
const (
	mainnetAPIBase = "https://pay.crypt.bot/api"
	testnetAPIBase = "https://testnet-pay.crypt.bot/api"
)

// BaseURL — адрес Crypto Pay API сети, выбранной конфигом.
func BaseURL(cfg config.PaymentConfig) string {
	if cfg.UseTestnet {
		return testnetAPIBase
	}
	return mainnetAPIBase
}
