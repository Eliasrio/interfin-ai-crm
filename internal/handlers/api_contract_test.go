// Контрактный тест REST API M8 (AQ²-1): каждый эндпоинт §4.1 отвечает
// корректным HTTP-статусом и форматом ошибок §4.2 ({"error","code"}).
// Роутер собран точно как в cmd/server: rate limit → JWT (M7) → роли §5.2.
// Хранилище — in-memory фейки; поведение БД (скоуп deleted_at, транзакция
// erasure) проверяют интеграционные тесты internal/repo против Postgres.
package handlers

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/kanban"
	"github.com/interfin/interfin-ai-crm/internal/lgpd"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// --- in-memory хранилище, разделяемое фейками репозиториев ---

type apiStore struct {
	mu       sync.Mutex
	leads    map[int64]*models.Lead
	msgs     map[int64][]models.Message
	payments map[int64][]models.PaymentEvent
	audit    []models.LGPDAudit
}

func newAPIStore(leads ...*models.Lead) *apiStore {
	s := &apiStore{
		leads:    map[int64]*models.Lead{},
		msgs:     map[int64][]models.Message{},
		payments: map[int64][]models.PaymentEvent{},
	}
	for _, l := range leads {
		s.leads[l.ID] = l
	}
	return s
}

type apiLeads struct{ s *apiStore }

func (f *apiLeads) Create(context.Context, *models.Lead) error {
	panic("не зовётся из /api")
}
func (f *apiLeads) GetByTelegramUserID(context.Context, int64) (*models.Lead, error) {
	panic("не зовётся из /api")
}
func (f *apiLeads) Save(context.Context, *models.Lead) error { panic("не зовётся из /api") }
func (f *apiLeads) UpdateFields(context.Context, int64, map[string]interface{}) error {
	panic("не зовётся из /api")
}
func (f *apiLeads) TransitionStage(context.Context, int64, int16, int16) (bool, error) {
	panic("не зовётся из /api (переходы — через StageMachine)")
}

// GetByID зеркалит soft-delete-скоуп GORM: стёртый лид невидим.
func (f *apiLeads) GetByID(_ context.Context, id int64) (*models.Lead, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	lead, ok := f.s.leads[id]
	if !ok || lead.DeletedAt.Valid {
		return nil, repo.ErrNotFound
	}
	cp := *lead
	return &cp, nil
}

func (f *apiLeads) List(_ context.Context, p repo.ListLeadsParams) ([]models.Lead, int64, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	var out []models.Lead
	for _, l := range f.s.leads {
		if l.DeletedAt.Valid {
			continue
		}
		if p.UpdatedSince != nil && !l.LastActivityAt.After(*p.UpdatedSince) {
			continue
		}
		out = append(out, *l)
	}
	total := int64(len(out))
	if p.Offset >= len(out) {
		return nil, total, nil
	}
	out = out[p.Offset:]
	if len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return out, total, nil
}

type apiMsgs struct{ s *apiStore }

func (f *apiMsgs) CreateInbound(context.Context, *models.Message) error {
	panic("не зовётся из /api")
}
func (f *apiMsgs) CreateOutbound(context.Context, *models.Message) error {
	panic("не зовётся из /api")
}
func (f *apiMsgs) ListByLead(_ context.Context, leadID int64, _ int) ([]models.Message, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	return append([]models.Message(nil), f.s.msgs[leadID]...), nil
}

type apiPayments struct{ s *apiStore }

func (f *apiPayments) Create(context.Context, *models.PaymentEvent) error {
	panic("не зовётся из /api")
}
func (f *apiPayments) ListByLead(_ context.Context, leadID int64) ([]models.PaymentEvent, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	return append([]models.PaymentEvent(nil), f.s.payments[leadID]...), nil
}

type apiLGPD struct{ s *apiStore }

func (f *apiLGPD) Erase(_ context.Context, leadID int64, hashedTgID int64, performedBy, ip string) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	lead, ok := f.s.leads[leadID]
	if !ok || lead.DeletedAt.Valid {
		return repo.ErrNotFound
	}
	lead.DeletedAt.Time, lead.DeletedAt.Valid = time.Now(), true
	lead.Name, lead.Phone, lead.TgUsername = nil, nil, nil
	lead.TelegramUserID = hashedTgID
	msgs := f.s.msgs[leadID]
	for i := range msgs {
		msgs[i].Content = lgpd.DeletedContent
	}
	f.s.audit = append(f.s.audit, models.LGPDAudit{
		Action: lgpd.ActionErase, LeadID: &leadID, PerformedBy: performedBy, IPAddress: &ip,
	})
	return nil
}

func (f *apiLGPD) GetLeadAny(_ context.Context, id int64) (*models.Lead, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	lead, ok := f.s.leads[id]
	if !ok {
		return nil, repo.ErrNotFound
	}
	cp := *lead
	return &cp, nil
}

func (f *apiLGPD) CreateAudit(_ context.Context, a *models.LGPDAudit) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	f.s.audit = append(f.s.audit, *a)
	return nil
}

func (f *apiLGPD) ListAuditByLead(_ context.Context, leadID int64) ([]models.LGPDAudit, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	var out []models.LGPDAudit
	for _, a := range f.s.audit {
		if a.LeadID != nil && *a.LeadID == leadID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *apiLGPD) DeleteErasedBefore(context.Context, time.Time) (int64, error) {
	panic("retention зовёт только воркер")
}

// apiMachine — StageMachine с поведением боевой M5: идемпотентный повтор,
// таблица §3.1, forcedErr — имитация исходов CAS/état (409 и пр.).
type apiMachine struct {
	s         *apiStore
	forcedErr error
}

func (m *apiMachine) Transition(_ context.Context, leadID int64, to int16, actor kanban.Actor, _ string) (*models.Lead, error) {
	if m.forcedErr != nil {
		return nil, m.forcedErr
	}
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	lead, ok := m.s.leads[leadID]
	if !ok || lead.DeletedAt.Valid {
		return nil, repo.ErrNotFound
	}
	if lead.StageID == to {
		cp := *lead
		return &cp, nil
	}
	if !kanban.CanTransition(lead.StageID, to, actor) {
		return nil, kanban.ErrInvalidTransition
	}
	lead.StageID = to
	cp := *lead
	return &cp, nil
}

// countingLimiter — детерминированный лимитер для юнит-уровня
// (Redis-реализацию проверяет ratelimit_integration_test.go).
type countingLimiter struct {
	mu    sync.Mutex
	n     int
	limit int
}

func (l *countingLimiter) Allow(context.Context, string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	return l.n <= l.limit, nil
}

// --- сборка роутера как в cmd/server ---

type apiRig struct {
	router  *gin.Engine
	store   *apiStore
	machine *apiMachine
	issuer  *auth.Issuer
	limiter *countingLimiter
}

func newAPIRig(t *testing.T, leads ...*models.Lead) *apiRig {
	t.Helper()
	gin.SetMode(gin.TestMode)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	store := newAPIStore(leads...)
	machine := &apiMachine{s: store}
	limiter := &countingLimiter{limit: 100}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	r := gin.New()
	api := r.Group("/api")
	api.Use(RateLimit(limiter, log))
	api.Use(
		auth.Middleware(auth.NewVerifier(&key.PublicKey)),
		auth.RequireRole(auth.RoleManager, auth.RoleAdmin),
	)
	NewLeads(LeadsDeps{
		Leads:    &apiLeads{s: store},
		Msgs:     &apiMsgs{s: store},
		Payments: &apiPayments{s: store},
		Machine:  machine,
		Log:      log,
	}).Register(api)
	NewLGPD(LGPDDeps{
		LGPD:     &apiLGPD{s: store},
		Msgs:     &apiMsgs{s: store},
		Payments: &apiPayments{s: store},
		Salt:     "test-salt",
		Log:      log,
	}).Register(api)

	return &apiRig{
		router:  r,
		store:   store,
		machine: machine,
		issuer:  auth.NewIssuer(key, time.Minute),
		limiter: limiter,
	}
}

func (rig *apiRig) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	rig.router.ServeHTTP(w, req)
	return w
}

func (rig *apiRig) token(t *testing.T, role string) string {
	t.Helper()
	token, err := rig.issuer.Issue("1", role)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("не JSON: %v; тело: %s", err, w.Body.String())
	}
	return m
}

func wantStatus(t *testing.T, w *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("статус %d, ждали %d; тело: %s", w.Code, status, w.Body.String())
	}
	m := decode(t, w)
	if code != "" {
		if got, _ := m["code"].(string); got != code {
			t.Fatalf("code %q, ждали %q; тело: %s", got, code, w.Body.String())
		}
		if msg, _ := m["error"].(string); msg == "" {
			t.Fatalf("ошибка без поля error (§4.2): %s", w.Body.String())
		}
	}
	return m
}

func strPtr(s string) *string { return &s }

func testLead(id int64, stage int16) *models.Lead {
	return &models.Lead{
		ID: id, TelegramUserID: 1000 + id, Name: strPtr("Иван"),
		Phone: strPtr("+5521999999999"), TgUsername: strPtr("ivan"),
		StageID: stage, MessageCount: 3,
		LastActivityAt: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-24 * time.Hour),
	}
}

// --- 401/403: auth-гейт на /api (§4.1, §5.2) ---

func TestAPIUnauthorized(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	for _, path := range []string{
		"/api/leads", "/api/leads/1", "/api/leads/1/stage", "/api/lgpd/leads/1/export",
	} {
		w := rig.do(t, http.MethodGet, path, "", nil)
		wantStatus(t, w, http.StatusUnauthorized, "ERR_TOKEN_INVALID")
	}
	w := rig.do(t, http.MethodGet, "/api/leads", "мусор-не-jwt", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("битый токен: статус %d, ждали 401", w.Code)
	}
}

func TestAPIForbiddenForSystemRole(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	token := rig.token(t, auth.RoleSystem) // system — только internal (§5.2)
	w := rig.do(t, http.MethodGet, "/api/leads", token, nil)
	wantStatus(t, w, http.StatusForbidden, "ERR_FORBIDDEN")
}

// --- GET /api/leads: пагинация и catch-up ---

func TestListLeads(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2), testLead(2, 4))
	token := rig.token(t, auth.RoleManager)

	m := wantStatus(t, rig.do(t, http.MethodGet, "/api/leads", token, nil), http.StatusOK, "")
	if got := m["total"].(float64); got != 2 {
		t.Fatalf("total = %v, ждали 2", got)
	}
	if got := len(m["leads"].([]any)); got != 2 {
		t.Fatalf("leads = %d, ждали 2", got)
	}

	// админу тоже можно (§5.2: admin — все leads).
	wantStatus(t, rig.do(t, http.MethodGet, "/api/leads", rig.token(t, auth.RoleAdmin), nil),
		http.StatusOK, "")
}

func TestListLeadsUpdatedSince(t *testing.T) {
	fresh, stale := testLead(1, 2), testLead(2, 2)
	fresh.LastActivityAt = time.Now()
	stale.LastActivityAt = time.Now().Add(-2 * time.Hour)
	rig := newAPIRig(t, fresh, stale)
	token := rig.token(t, auth.RoleManager)

	since := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	m := wantStatus(t, rig.do(t, http.MethodGet, "/api/leads?updated_since="+since, token, nil),
		http.StatusOK, "")
	leads := m["leads"].([]any)
	if len(leads) != 1 {
		t.Fatalf("catch-up вернул %d лидов, ждали 1 (только свежего)", len(leads))
	}
	if id := leads[0].(map[string]any)["id"].(float64); id != 1 {
		t.Fatalf("catch-up вернул лида %v, ждали 1", id)
	}
}

func TestListLeadsValidation(t *testing.T) {
	rig := newAPIRig(t)
	token := rig.token(t, auth.RoleManager)
	for _, q := range []string{
		"?limit=0", "?limit=-5", "?limit=201", "?limit=abc",
		"?offset=-1", "?offset=x",
		"?updated_since=не-время", "?updated_since=1751800000",
	} {
		w := rig.do(t, http.MethodGet, "/api/leads"+q, token, nil)
		wantStatus(t, w, http.StatusBadRequest, "ERR_VALIDATION")
	}
}

// --- GET /api/leads/:id ---

func TestGetLeadCard(t *testing.T) {
	lead := testLead(1, 2)
	rig := newAPIRig(t, lead)
	rig.store.msgs[1] = []models.Message{
		{ID: 1, LeadID: 1, Direction: models.DirectionInbound, Content: "привет"},
		{ID: 2, LeadID: 1, Direction: models.DirectionOutbound, Content: "здравствуйте"},
	}
	rig.store.payments[1] = []models.PaymentEvent{{ID: 1, LeadID: 1}}
	token := rig.token(t, auth.RoleManager)

	m := wantStatus(t, rig.do(t, http.MethodGet, "/api/leads/1", token, nil), http.StatusOK, "")
	if m["lead"].(map[string]any)["id"].(float64) != 1 {
		t.Fatalf("карточка не о том лиде: %v", m["lead"])
	}
	if len(m["messages"].([]any)) != 2 || len(m["payment_events"].([]any)) != 1 {
		t.Fatalf("карточка без диалога/платежей: %s", "ok")
	}

	wantStatus(t, rig.do(t, http.MethodGet, "/api/leads/999", token, nil),
		http.StatusNotFound, "ERR_NOT_FOUND")
	wantStatus(t, rig.do(t, http.MethodGet, "/api/leads/abc", token, nil),
		http.StatusBadRequest, "ERR_VALIDATION")
}

// --- PATCH /api/leads/:id/stage (через state machine M5) ---

func TestPatchStage(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	token := rig.token(t, auth.RoleManager)

	m := wantStatus(t, rig.do(t, http.MethodPatch, "/api/leads/1/stage", token,
		gin.H{"stage_id": 5}), http.StatusOK, "")
	if got := m["lead"].(map[string]any)["stage_id"].(float64); got != 5 {
		t.Fatalf("stage_id = %v, ждали 5", got)
	}

	// Идемпотентный повтор (double click) — 200, не 409 (контракт M5).
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/leads/1/stage", token,
		gin.H{"stage_id": 5}), http.StatusOK, "")
}

func TestPatchStageValidation(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	token := rig.token(t, auth.RoleManager)

	for name, body := range map[string]any{
		"без stage_id":       gin.H{},
		"stage_id вне доски": gin.H{"stage_id": 9},
		"stage_id нулевой":   gin.H{"stage_id": 0},
		"не-число":           gin.H{"stage_id": "пять"},
	} {
		w := rig.do(t, http.MethodPatch, "/api/leads/1/stage", token, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: статус %d, ждали 400; тело: %s", name, w.Code, w.Body.String())
		}
	}

	wantStatus(t, rig.do(t, http.MethodPatch, "/api/leads/999/stage", token,
		gin.H{"stage_id": 5}), http.StatusNotFound, "ERR_NOT_FOUND")
}

func TestPatchStageMachineErrors(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	token := rig.token(t, auth.RoleManager)

	// ErrInvalidTransition → 400 (контракт M5→M8).
	rig.machine.forcedErr = fmt.Errorf("kanban: manager: 2 → 7: %w", kanban.ErrInvalidTransition)
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/leads/1/stage", token,
		gin.H{"stage_id": 7}), http.StatusBadRequest, "ERR_INVALID_TRANSITION")

	// ErrStageConflict → 409 state conflict (§4.2).
	rig.machine.forcedErr = fmt.Errorf("kanban: manager: 2 → 5: %w", kanban.ErrStageConflict)
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/leads/1/stage", token,
		gin.H{"stage_id": 5}), http.StatusConflict, "ERR_STAGE_CONFLICT")
}

// --- GET /api/leads/:id/stage (polling fallback §10.1) ---

func TestGetStage(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 4))
	token := rig.token(t, auth.RoleManager)

	m := wantStatus(t, rig.do(t, http.MethodGet, "/api/leads/1/stage", token, nil),
		http.StatusOK, "")
	if got := m["stage_id"].(float64); got != 4 {
		t.Fatalf("stage_id = %v, ждали 4", got)
	}
	wantStatus(t, rig.do(t, http.MethodGet, "/api/leads/999/stage", token, nil),
		http.StatusNotFound, "ERR_NOT_FOUND")
}

// --- LGPD (§9): erase и export ---

func TestLGPDErase(t *testing.T) {
	lead := testLead(1, 2)
	origTgID := lead.TelegramUserID
	rig := newAPIRig(t, lead)
	rig.store.msgs[1] = []models.Message{{ID: 1, LeadID: 1, Content: "секретные данные"}}
	token := rig.token(t, auth.RoleManager)

	m := wantStatus(t, rig.do(t, http.MethodDelete, "/api/lgpd/leads/1/erase", token, nil),
		http.StatusOK, "")
	if note, _ := m["financial_records"].(string); note == "" {
		t.Fatal("в ответе erasure нет пометки о сохранении финансовых записей (§9.3)")
	}

	rig.store.mu.Lock()
	got := rig.store.leads[1]
	if !got.DeletedAt.Valid || got.Name != nil || got.Phone != nil || got.TgUsername != nil {
		t.Fatalf("лид не анонимизирован: %+v", got)
	}
	if got.TelegramUserID == origTgID || got.TelegramUserID >= 0 {
		t.Fatalf("telegram_user_id не хеширован: %d", got.TelegramUserID)
	}
	if rig.store.msgs[1][0].Content != lgpd.DeletedContent {
		t.Fatalf("messages.content = %q, ждали %q", rig.store.msgs[1][0].Content, lgpd.DeletedContent)
	}
	rig.store.mu.Unlock()

	// Повторный erase → 404 (уже стёрт), стёртый лид невидим для /api/leads/:id.
	wantStatus(t, rig.do(t, http.MethodDelete, "/api/lgpd/leads/1/erase", token, nil),
		http.StatusNotFound, "ERR_NOT_FOUND")
	wantStatus(t, rig.do(t, http.MethodGet, "/api/leads/1", token, nil),
		http.StatusNotFound, "ERR_NOT_FOUND")

	wantStatus(t, rig.do(t, http.MethodDelete, "/api/lgpd/leads/999/erase", token, nil),
		http.StatusNotFound, "ERR_NOT_FOUND")
}

func TestLGPDExport(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	rig.store.msgs[1] = []models.Message{{ID: 1, LeadID: 1, Content: "привет"}}
	rig.store.payments[1] = []models.PaymentEvent{{ID: 1, LeadID: 1}}
	token := rig.token(t, auth.RoleAdmin)

	m := wantStatus(t, rig.do(t, http.MethodGet, "/api/lgpd/leads/1/export", token, nil),
		http.StatusOK, "")
	if len(m["messages"].([]any)) != 1 || len(m["payment_events"].([]any)) != 1 {
		t.Fatal("export без messages/payment_events (§9.1)")
	}
	if note, _ := m["financial_records"].(string); note == "" {
		t.Fatal("в export нет пометки о сохранении финансовых записей (§9.3)")
	}
	// Сам export оставил след в audit (§9.2) и виден в повторном export.
	audit := m["lgpd_audit"].([]any)
	if len(audit) != 1 || audit[0].(map[string]any)["action"].(string) != lgpd.ActionExport {
		t.Fatalf("след export в lgpd_audit не записан: %v", audit)
	}

	// Export работает и после erasure (право на доступ не гаснет).
	wantStatus(t, rig.do(t, http.MethodDelete, "/api/lgpd/leads/1/erase", token, nil),
		http.StatusOK, "")
	m = wantStatus(t, rig.do(t, http.MethodGet, "/api/lgpd/leads/1/export", token, nil),
		http.StatusOK, "")
	if erased, _ := m["erased"].(bool); !erased {
		t.Fatal("export после erasure не помечен erased=true")
	}

	wantStatus(t, rig.do(t, http.MethodGet, "/api/lgpd/leads/999/export", rig.token(t, auth.RoleManager), nil),
		http.StatusNotFound, "ERR_NOT_FOUND")
}

// --- 429: rate limit §4.2 (критерий M8: срабатывает на 101-м запросе) ---

func TestRateLimit101st(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	token := rig.token(t, auth.RoleManager)

	for i := 0; i < 100; i++ {
		if w := rig.do(t, http.MethodGet, "/api/leads", token, nil); w.Code != http.StatusOK {
			t.Fatalf("запрос %d: статус %d, ждали 200; тело: %s", i+1, w.Code, w.Body.String())
		}
	}
	// 101-й — 429; заодно фиксируем порядок цепочки: лимит стоит ДО auth,
	// поэтому 429 отдаётся даже без токена.
	w := rig.do(t, http.MethodGet, "/api/leads", "", nil)
	wantStatus(t, w, http.StatusTooManyRequests, "ERR_RATE_LIMITED")
}
