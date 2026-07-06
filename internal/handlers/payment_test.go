package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/kanban"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/payment"
)

const payTestToken = "59254:AAtestnetToken"

// invoicePaidJSON — вебхук invoice_paid CryptoBot: лид leadID, инвойс на due
// USDT, оплачено paid, комиссия шлюза fee (net = paid − fee, §3.3).
func invoicePaidJSON(updateID, leadID int64, due, paid, fee string) string {
	return fmt.Sprintf(`{
		"update_id": %d,
		"update_type": "invoice_paid",
		"request_date": %q,
		"payload": {
			"invoice_id": 528,
			"status": "paid",
			"asset": "USDT",
			"amount": %q,
			"paid_amount": %q,
			"fee_asset": "USDT",
			"fee_amount": %q,
			"payload": "%d"
		}
	}`, updateID, time.Now().UTC().Format(time.RFC3339), due, paid, fee, leadID)
}

// --- фейки платёжных зависимостей (fakeLeads переиспользуется из telegram_test) ---

type fakePayments struct {
	events    []*models.PaymentEvent
	createErr error
}

func (f *fakePayments) Create(_ context.Context, ev *models.PaymentEvent) error {
	if f.createErr != nil {
		return f.createErr
	}
	ev.ID = int64(len(f.events) + 1)
	f.events = append(f.events, ev)
	return nil
}

func (f *fakePayments) ListByLead(_ context.Context, _ int64) ([]models.PaymentEvent, error) {
	return nil, nil
}

type transitionCall struct {
	LeadID int64
	To     int16
	Actor  kanban.Actor
}

type fakeMachine struct {
	calls []transitionCall
	errs  []error // очередь ошибок: errs[i] — ответ i-го вызова
}

func (f *fakeMachine) Transition(_ context.Context, leadID int64, to int16, actor kanban.Actor, _ string) (*models.Lead, error) {
	f.calls = append(f.calls, transitionCall{LeadID: leadID, To: to, Actor: actor})
	if n := len(f.calls) - 1; n < len(f.errs) && f.errs[n] != nil {
		return nil, f.errs[n]
	}
	return &models.Lead{ID: leadID, StageID: to}, nil
}

type fakeNonces struct {
	claimed  map[string]bool
	released []string
	claimErr error
}

func newFakeNonces() *fakeNonces { return &fakeNonces{claimed: map[string]bool{}} }

func (f *fakeNonces) Claim(_ context.Context, nonce string, _ time.Duration) (bool, error) {
	if f.claimErr != nil {
		return false, f.claimErr
	}
	if f.claimed[nonce] {
		return false, nil
	}
	f.claimed[nonce] = true
	return true, nil
}

func (f *fakeNonces) Release(_ context.Context, nonce string) error {
	delete(f.claimed, nonce)
	f.released = append(f.released, nonce)
	return nil
}

type fakePub struct{ events []events.Event }

func (f *fakePub) Publish(_ context.Context, ev events.Event) error {
	f.events = append(f.events, ev)
	return nil
}

type payEnv struct {
	router   *gin.Engine
	leads    *fakeLeads
	payments *fakePayments
	machine  *fakeMachine
	nonces   *fakeNonces
	pub      *fakePub
}

// newPayEnv собирает цепочку с лидом 101 в Stage 2 и tolerance pct из конфига.
func newPayEnv(t *testing.T, tolerancePct float64) *payEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := &payEnv{
		leads:    newFakeLeads(),
		payments: &fakePayments{},
		machine:  &fakeMachine{},
		nonces:   newFakeNonces(),
		pub:      &fakePub{},
	}
	e.leads.byTgID[777] = &models.Lead{ID: 101, TelegramUserID: 777, StageID: kanban.StageLive}
	e.router = gin.New()
	NewPaymentWebhook(PaymentWebhookDeps{
		Leads:    e.leads,
		Payments: e.payments,
		Machine:  e.machine,
		Nonces:   e.nonces,
		Pub:      e.pub,
		Cfg: config.PaymentConfig{
			Gateway:             "cryptobot",
			TestnetToken:        payTestToken,
			UseTestnet:          true,
			ReplayWindowMinutes: 5,
		},
		TolerancePct: tolerancePct,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Register(e.router)
	return e
}

// post шлёт вебхук; sign=true — с валидной подписью активного токена.
func (e *payEnv) post(body string, sign bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhook/payment", bytes.NewBufferString(body))
	if sign {
		req.Header.Set(payment.SignatureHeader, payment.Sign(payTestToken, []byte(body)))
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// --- критерий приёмки 1: невалидная подпись → 403, повтор nonce → отклонён ---

func TestPayment_NoSignature403(t *testing.T) {
	e := newPayEnv(t, 2.0)
	w := e.post(invoicePaidJSON(1, 101, "100", "100", "0"), false)
	if w.Code != http.StatusForbidden {
		t.Fatalf("без подписи ожидали 403, получили %d", w.Code)
	}
	if len(e.payments.events) != 0 || len(e.machine.calls) != 0 {
		t.Error("неподписанный запрос не должен доходить до записи/переходов")
	}
}

func TestPayment_InvalidSignature403(t *testing.T) {
	e := newPayEnv(t, 2.0)
	body := invoicePaidJSON(1, 101, "100", "100", "0")
	req := httptest.NewRequest(http.MethodPost, "/webhook/payment", bytes.NewBufferString(body))
	req.Header.Set(payment.SignatureHeader, payment.Sign("999:wrongToken", []byte(body)))
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("с чужой подписью ожидали 403, получили %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("ERR_PAYMENT_FORBIDDEN")) {
		t.Errorf("ошибка должна быть в формате {error, code}: %s", w.Body.String())
	}
}

func TestPayment_MainnetTokenDoesNotSignTestnet(t *testing.T) {
	// Переключатель сетей: хендлер работает в testnet — подпись mainnet-токеном
	// невалидна, даже если он «наш».
	e := newPayEnv(t, 2.0)
	body := invoicePaidJSON(1, 101, "100", "100", "0")
	req := httptest.NewRequest(http.MethodPost, "/webhook/payment", bytes.NewBufferString(body))
	req.Header.Set(payment.SignatureHeader, payment.Sign("605771:mainnetToken", []byte(body)))
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("подпись токеном другой сети должна давать 403, получили %d", w.Code)
	}
}

func TestPayment_NonceReplay403(t *testing.T) {
	e := newPayEnv(t, 2.0)
	body := invoicePaidJSON(7, 101, "100", "100", "0")

	if w := e.post(body, true); w.Code != http.StatusOK {
		t.Fatalf("первая доставка: ожидали 200, получили %d (%s)", w.Code, w.Body.String())
	}
	w := e.post(body, true)
	if w.Code != http.StatusForbidden {
		t.Fatalf("повтор nonce: ожидали 403, получили %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("ERR_PAYMENT_REPLAY")) {
		t.Errorf("код ошибки replay: %s", w.Body.String())
	}
	if len(e.payments.events) != 1 || len(e.machine.calls) != 1 {
		t.Errorf("replay не должен обрабатываться повторно: events=%d transitions=%d",
			len(e.payments.events), len(e.machine.calls))
	}
}

func TestPayment_StaleTimestamp403(t *testing.T) {
	e := newPayEnv(t, 2.0)
	stale := fmt.Sprintf(`{
		"update_id": 8, "update_type": "invoice_paid", "request_date": %q,
		"payload": {"invoice_id": 1, "status": "paid", "asset": "USDT",
			"amount": "100", "payload": "101"}
	}`, time.Now().UTC().Add(-10*time.Minute).Format(time.RFC3339))

	w := e.post(stale, true)
	if w.Code != http.StatusForbidden {
		t.Fatalf("request_date старше окна ±5 мин: ожидали 403, получили %d", w.Code)
	}
	if len(e.nonces.claimed) != 0 {
		t.Error("устаревший запрос не должен занимать nonce")
	}
}

// --- критерии приёмки 2–3: tolerance §3.3, pct из конфига ---

func TestPayment_99of100_Stage3(t *testing.T) {
	// IQ-3: USDT 99 из 100 → tolerance_ok → Stage 3 (Оплачено: ждут ссылку).
	e := newPayEnv(t, 2.0)
	w := e.post(invoicePaidJSON(10, 101, "100", "99.5", "0.5"), true) // net = 99

	if w.Code != http.StatusOK {
		t.Fatalf("ожидали 200, получили %d (%s)", w.Code, w.Body.String())
	}
	if len(e.machine.calls) != 1 {
		t.Fatalf("ожидали один переход, получили %d", len(e.machine.calls))
	}
	call := e.machine.calls[0]
	if call != (transitionCall{LeadID: 101, To: kanban.StagePaid, Actor: kanban.ActorPayment}) {
		t.Errorf("переход неверный: %+v (ожидали лид 101 → Stage 3 от ActorPayment)", call)
	}
	if len(e.payments.events) != 1 || e.payments.events[0].ToleranceOk == nil || !*e.payments.events[0].ToleranceOk {
		t.Error("tolerance_ok должен быть true при недоплате в пределах 2%")
	}
	fields, ok := e.leads.updates[101]
	if !ok || fields["manual_resolution"] != false {
		t.Errorf("успешная оплата должна снимать manual_resolution (false), получили %v", e.leads.updates)
	}
}

func TestPayment_97of100_Stage4ManualResolution(t *testing.T) {
	// IQ-3: USDT 97 из 100 → недоплата → manual_resolution + Stage 4 (TTL 48ч
	// взводит машина стадий — её контракт проверен в M5).
	e := newPayEnv(t, 2.0)
	w := e.post(invoicePaidJSON(11, 101, "100", "97.5", "0.5"), true) // net = 97

	if w.Code != http.StatusOK {
		t.Fatalf("ожидали 200, получили %d (%s)", w.Code, w.Body.String())
	}
	call := e.machine.calls[0]
	if call.To != kanban.StageUnpaid || call.Actor != kanban.ActorPayment {
		t.Errorf("недоплата должна вести в Stage 4: %+v", call)
	}
	fields, ok := e.leads.updates[101]
	if !ok || fields["manual_resolution"] != true {
		t.Errorf("недоплата должна ставить manual_resolution=true (§3.3), получили %v", e.leads.updates)
	}
	ev := e.payments.events[0]
	if ev.ToleranceOk == nil || *ev.ToleranceOk {
		t.Error("tolerance_ok должен быть false при 97 из 100")
	}
}

func TestPayment_ExactBoundary98_Stage3(t *testing.T) {
	// Граница §3.3 включительная: net = ровно 98% → tolerance_ok.
	e := newPayEnv(t, 2.0)
	e.post(invoicePaidJSON(12, 101, "100", "98", "0"), true)
	if e.machine.calls[0].To != kanban.StagePaid {
		t.Errorf("net = ровно 98%% — это ещё tolerance_ok (>=), получили Stage %d", e.machine.calls[0].To)
	}
}

func TestPayment_TolerancePctFromConfig(t *testing.T) {
	// Критерий 3: tolerance_pct берётся из конфига, не из хардкода 2%.
	// При pct=5 те же 97 из 100 проходят в Stage 3.
	e := newPayEnv(t, 5.0)
	e.post(invoicePaidJSON(13, 101, "100", "97.5", "0.5"), true) // net = 97
	if got := e.machine.calls[0].To; got != kanban.StagePaid {
		t.Errorf("при tolerance 5%% net=97 должен давать Stage 3, получили Stage %d", got)
	}
}

// --- критерий приёмки 4: payment_events заполнен корректно ---

func TestPayment_EventFieldsRecorded(t *testing.T) {
	e := newPayEnv(t, 2.0)
	body := invoicePaidJSON(14, 101, "100", "99.5", "0.5")
	e.post(body, true)

	if len(e.payments.events) != 1 {
		t.Fatalf("ожидали одну запись payment_events, получили %d", len(e.payments.events))
	}
	ev := e.payments.events[0]

	if ev.LeadID != 101 {
		t.Errorf("lead_id = %d", ev.LeadID)
	}
	if ev.Gateway == nil || *ev.Gateway != "cryptobot" {
		t.Errorf("gateway = %v", ev.Gateway)
	}
	if ev.Status == nil || *ev.Status != "paid" {
		t.Errorf("status = %v", ev.Status)
	}
	if !ev.AmountDue.Decimal.Equal(decimal.NewFromInt(100)) {
		t.Errorf("amount_due = %s, ожидали 100", ev.AmountDue.Decimal)
	}
	if !ev.AmountReceived.Decimal.Equal(decimal.RequireFromString("99.5")) {
		t.Errorf("amount_received = %s, ожидали 99.5", ev.AmountReceived.Decimal)
	}
	// net_received учитывает комиссию шлюза: 99.5 − 0.5 = 99.
	if !ev.NetReceived.Decimal.Equal(decimal.NewFromInt(99)) {
		t.Errorf("net_received = %s, ожидали 99 (за вычетом fee)", ev.NetReceived.Decimal)
	}
	if ev.Currency == nil || *ev.Currency != "USDT" {
		t.Errorf("currency = %v", ev.Currency)
	}
	// raw_payload — сырое тело вебхука (аудит и LGPD export M8).
	var stored map[string]interface{}
	if err := json.Unmarshal(ev.RawPayload, &stored); err != nil || stored["update_id"] != float64(14) {
		t.Errorf("raw_payload должен хранить исходное тело: %v %s", err, string(ev.RawPayload))
	}
}

// --- задача 6: WS-событие payment_received ---

func TestPayment_PublishesPaymentReceived(t *testing.T) {
	e := newPayEnv(t, 2.0)
	e.post(invoicePaidJSON(15, 101, "100", "99.5", "0.5"), true)

	if len(e.pub.events) != 1 {
		t.Fatalf("ожидали одно событие crm:events, получили %d", len(e.pub.events))
	}
	ev := e.pub.events[0]
	if ev.Type != events.TypePaymentReceived {
		t.Errorf("type = %q, ожидали payment_received", ev.Type)
	}
	if ev.LeadID != 101 || ev.StageID != kanban.StagePaid {
		t.Errorf("событие должно нести лида и стадию ПОСЛЕ перехода: %+v", ev)
	}
	if ev.Amount != "99" || ev.Currency != "USDT" {
		t.Errorf("amount/currency = %q/%q, ожидали 99/USDT (net)", ev.Amount, ev.Currency)
	}
	if ev.ToleranceOk == nil || !*ev.ToleranceOk {
		t.Errorf("tolerance_ok в событии = %v", ev.ToleranceOk)
	}
}

// --- прочие ветки транспорта ---

func TestPayment_NonInvoicePaidIgnored(t *testing.T) {
	e := newPayEnv(t, 2.0)
	body := fmt.Sprintf(`{"update_id": 16, "update_type": "invoice_expired", "request_date": %q, "payload": {}}`,
		time.Now().UTC().Format(time.RFC3339))
	w := e.post(body, true)
	if w.Code != http.StatusOK {
		t.Fatalf("незнакомый тип апдейта: ожидали 200, получили %d", w.Code)
	}
	if len(e.payments.events) != 0 || len(e.machine.calls) != 0 {
		t.Error("не-invoice_paid не должен ничего записывать")
	}
}

func TestPayment_UnknownLeadIgnored(t *testing.T) {
	e := newPayEnv(t, 2.0)
	w := e.post(invoicePaidJSON(17, 999, "100", "100", "0"), true) // лида 999 нет
	if w.Code != http.StatusOK {
		t.Fatalf("неизвестный лид: 200 (ретраи не исправят), получили %d", w.Code)
	}
	if len(e.payments.events) != 0 || len(e.machine.calls) != 0 {
		t.Error("платёж неизвестного лида не должен записываться/двигать стадии")
	}
}

func TestPayment_ProcessingFailureReleasesNonce(t *testing.T) {
	// Провал после захвата nonce → 5xx + nonce возвращён: ретрай шлюза
	// не должен быть отвергнут как replay.
	e := newPayEnv(t, 2.0)
	e.payments.createErr = errors.New("db down")

	body := invoicePaidJSON(18, 101, "100", "100", "0")
	w := e.post(body, true)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("провал записи: ожидали 500 (шлюз ретраит), получили %d", w.Code)
	}
	if len(e.nonces.released) != 1 || e.nonces.released[0] != "18" {
		t.Errorf("nonce должен быть возвращён после провала: %v", e.nonces.released)
	}

	// Ретрай шлюза после починки БД проходит.
	e.payments.createErr = nil
	if w := e.post(body, true); w.Code != http.StatusOK {
		t.Errorf("ретрай после провала должен пройти: %d (%s)", w.Code, w.Body.String())
	}
}

func TestPayment_StageConflictRetried(t *testing.T) {
	// ErrStageConflict = конкурирующий переход; Payment переопределяет
	// авто-триггеры (§3.1) — пробуем ещё раз, а не отдаём 5xx.
	e := newPayEnv(t, 2.0)
	e.machine.errs = []error{kanban.ErrStageConflict, nil}

	w := e.post(invoicePaidJSON(19, 101, "100", "100", "0"), true)
	if w.Code != http.StatusOK {
		t.Fatalf("после ретрая конфликта ожидали 200, получили %d", w.Code)
	}
	if len(e.machine.calls) != 2 {
		t.Errorf("ожидали 2 попытки перехода, получили %d", len(e.machine.calls))
	}
}
