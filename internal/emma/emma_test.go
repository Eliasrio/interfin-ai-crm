// Контрактные тесты EP-01 (по образцу handlers/api_contract_test.go):
// роутер собран как в cmd/server — rate limit тут не участвует, цепочка
// JWT (M7) → RequireRole(manager|admin) на /api → RequireRole(admin) +
// Audit на /api/emma → pin/* без RequirePIN, остальное за RequirePIN.
//
// Redis — in-memory фейк (боевой RedisStore проверяет
// redis_integration_test.go при REDIS_TEST_ADDR); settings — боевой
// сервис поверх фейкового репозитория (кэш — часть контракта).
package emma

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/repo"
	"github.com/interfin/interfin-ai-crm/internal/settings"
)

// --- фейки ---

// memSettingsRepo — in-memory repo.SettingsRepo (как apiSettingsRepo M13).
type memSettingsRepo struct {
	mu     sync.Mutex
	values map[string]string
}

func (f *memSettingsRepo) Get(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.values[key]
	if !ok {
		return "", repo.ErrNotFound
	}
	return v, nil
}

func (f *memSettingsRepo) Set(_ context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[key] = value
	return nil
}

// fakeStore — in-memory Store. TTL не тикает: истечение окна брутфорса
// имитируется ResetFails/удалением (реальные TTL — интеграционный тест).
type fakeStore struct {
	mu       sync.Mutex
	sessions map[string]bool
	fails    map[string]int64
	touches  int // сколько раз продлевали сессию (sliding)
}

func newFakeStore() *fakeStore {
	return &fakeStore{sessions: map[string]bool{}, fails: map[string]int64{}}
}

func (s *fakeStore) OpenSession(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = true
	return nil
}

func (s *fakeStore) TouchSession(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sessions[id] {
		return false, nil
	}
	s.touches++
	return true, nil
}

func (s *fakeStore) SessionActive(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id], nil
}

func (s *fakeStore) CloseSession(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	return nil
}

func (s *fakeStore) FailState(_ context.Context, id string) (int64, time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.fails[id]
	if n == 0 {
		return 0, 0, nil
	}
	return n, FailWindow, nil
}

func (s *fakeStore) IncrFail(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails[id]++
	return nil
}

func (s *fakeStore) ResetFails(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.fails, id)
	return nil
}

func (s *fakeStore) PurgeAll(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := int64(len(s.sessions) + len(s.fails))
	s.sessions = map[string]bool{}
	s.fails = map[string]int64{}
	return n, nil
}

// brokenStore — Redis лёг: каждая операция — ошибка (ветки fail-closed §2.2).
type brokenStore struct{}

var errRedisDown = errors.New("redis: connection refused")

func (brokenStore) OpenSession(context.Context, string) error { return errRedisDown }
func (brokenStore) TouchSession(context.Context, string) (bool, error) {
	return false, errRedisDown
}
func (brokenStore) SessionActive(context.Context, string) (bool, error) {
	return false, errRedisDown
}
func (brokenStore) CloseSession(context.Context, string) error { return errRedisDown }
func (brokenStore) FailState(context.Context, string) (int64, time.Duration, error) {
	return 0, 0, errRedisDown
}
func (brokenStore) IncrFail(context.Context, string) error   { return errRedisDown }
func (brokenStore) ResetFails(context.Context, string) error { return errRedisDown }

// --- риг ---

type rig struct {
	router    *gin.Engine
	issuer    *auth.Issuer
	store     Store
	settings  *settings.Service
	repo      *memSettingsRepo
	prompts   *memPromptRepo    // EP-02
	kb        *memKBRepo        // EP-03
	kbEnq     *fakeKBEnqueuer   // EP-03
	kbDir     string            // EP-03: t.TempDir()
	sendFiles *memSendFilesRepo // EP-04
	filesDir  string            // EP-04: t.TempDir()
}

func newRig(t *testing.T, store Store) *rig {
	t.Helper()
	gin.SetMode(gin.TestMode)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	memRepo := &memSettingsRepo{values: map[string]string{}}
	settingsSvc := settings.New(memRepo)

	r := gin.New()
	api := r.Group("/api")
	api.Use(
		auth.Middleware(auth.NewVerifier(&key.PublicKey)),
		auth.RequireRole(auth.RoleManager, auth.RoleAdmin),
	)
	// Сборка группы — как в cmd/server (это и есть проверяемый контракт).
	emmaGroup := api.Group("/emma", auth.RequireRole(auth.RoleAdmin), Audit(log))
	NewPIN(PINDeps{Settings: settingsSvc, Store: store, Log: log}).
		Register(emmaGroup.Group("/pin"))
	protected := emmaGroup.Group("", RequirePIN(store, log))
	NewStatus(StatusInfo{
		TelegramTokenSet: true,
		AnthropicKeySet:  true,
		Model:            "claude-sonnet-5",
		WebhookURLSet:    false,
	}).Register(protected)
	// EP-02: промпт + история — на той же защищённой группе.
	prompts := newMemPromptRepo()
	NewPrompt(PromptDeps{Prompts: prompts, Settings: settingsSvc, Log: log}).
		Register(protected)
	// EP-03: база знаний — на той же защищённой группе.
	kbRepo := newMemKBRepo()
	kbEnq := &fakeKBEnqueuer{}
	kbDir := t.TempDir()
	NewKB(KBDeps{Files: kbRepo, Enq: kbEnq, KBDir: kbDir, Log: log}).
		Register(protected)
	// EP-04: файлы для отправки — на той же защищённой группе.
	sendFiles := newMemSendFilesRepo()
	filesDir := t.TempDir()
	NewFiles(FilesDeps{Files: sendFiles, Dir: filesDir, Log: log}).
		Register(protected)

	return &rig{
		router:    r,
		issuer:    auth.NewIssuer(key, time.Minute),
		store:     store,
		settings:  settingsSvc,
		repo:      memRepo,
		prompts:   prompts,
		kb:        kbRepo,
		kbEnq:     kbEnq,
		kbDir:     kbDir,
		sendFiles: sendFiles,
		filesDir:  filesDir,
	}
}

func (rg *rig) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
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
	rg.router.ServeHTTP(w, req)
	return w
}

func (rg *rig) token(t *testing.T, role string) string {
	t.Helper()
	token, err := rg.issuer.Issue("1", role)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func wantStatus(t *testing.T, w *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("статус %d, ждали %d; тело: %s", w.Code, status, w.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("не JSON: %v; тело: %s", err, w.Body.String())
	}
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

// setupPIN — быстрый bootstrap до состояния «PIN задан, сессия открыта».
func setupPIN(t *testing.T, rg *rig, admin, pin string) {
	t.Helper()
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/setup", admin,
		gin.H{"pin": pin}), http.StatusOK, "")
}

// emmaPaths — все роуты EP-01 + EP-02 (гейт-тесты обязаны покрывать каждый:
// manager → 403, без токена → 401, admin без PIN-сессии → 401 PIN_REQUIRED).
var emmaPaths = []struct{ method, path string }{
	{http.MethodGet, "/api/emma/pin/status"},
	{http.MethodPost, "/api/emma/pin/setup"},
	{http.MethodPost, "/api/emma/pin/verify"},
	{http.MethodPost, "/api/emma/pin/change"},
	{http.MethodDelete, "/api/emma/pin/session"},
	{http.MethodGet, "/api/emma/status"},
	// EP-02 (критерий приёмки: все ручки prompt за общей цепочкой гейтов).
	{http.MethodGet, "/api/emma/prompt"},
	{http.MethodPut, "/api/emma/prompt"},
	{http.MethodGet, "/api/emma/prompt/history"},
	{http.MethodGet, "/api/emma/prompt/history/1"},
	{http.MethodPost, "/api/emma/prompt/history/1/restore"},
	// EP-03 (критерий приёмки: manager → 403, admin без PIN → 401 на всех
	// ручках kb).
	{http.MethodGet, "/api/emma/kb"},
	{http.MethodPost, "/api/emma/kb"},
	{http.MethodPost, "/api/emma/kb/1/reindex"},
	{http.MethodDelete, "/api/emma/kb/1"},
	// EP-04 (критерий приёмки: manager → 403, admin без PIN → 401 на всех
	// ручках files).
	{http.MethodGet, "/api/emma/files"},
	{http.MethodPost, "/api/emma/files"},
	{http.MethodPatch, "/api/emma/files/1"},
	{http.MethodDelete, "/api/emma/files/1"},
}

// --- гейты: роль и PIN-сессия (критерий приёмки 2) ---

func TestManagerForbiddenEverywhere(t *testing.T) {
	rg := newRig(t, newFakeStore())
	manager := rg.token(t, auth.RoleManager)
	for _, p := range emmaPaths {
		w := rg.do(t, p.method, p.path, manager, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s: manager получил %d, ждали 403", p.method, p.path, w.Code)
		}
	}
}

func TestUnauthenticated401(t *testing.T) {
	rg := newRig(t, newFakeStore())
	for _, p := range emmaPaths {
		w := rg.do(t, p.method, p.path, "", nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: без токена %d, ждали 401", p.method, p.path, w.Code)
		}
	}
}

func TestAdminWithoutSessionGetsPinRequired(t *testing.T) {
	rg := newRig(t, newFakeStore())
	admin := rg.token(t, auth.RoleAdmin)
	w := rg.do(t, http.MethodGet, "/api/emma/status", admin, nil)
	wantStatus(t, w, http.StatusUnauthorized, CodePinRequired)
	// pin/status при этом доступен — на нём RequirePIN не стоит.
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/pin/status", admin, nil),
		http.StatusOK, "")
}

// --- bootstrap (критерий приёмки 3) ---

func TestBootstrapFlow(t *testing.T) {
	rg := newRig(t, newFakeStore())
	admin := rg.token(t, auth.RoleAdmin)

	// До установки: pin_set=false, session_active=false.
	m := wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/pin/status", admin, nil),
		http.StatusOK, "")
	if m["pin_set"].(bool) || m["session_active"].(bool) {
		t.Fatalf("до setup: %v", m)
	}

	// setup «123456» → 200, сессия открыта сразу (владелец не вводит дважды).
	m = wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/setup", admin,
		gin.H{"pin": "123456"}), http.StatusOK, "")
	if !m["pin_set"].(bool) || !m["session_active"].(bool) {
		t.Fatalf("после setup: %v", m)
	}
	// Защищённая группа проходит без verify.
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/status", admin, nil),
		http.StatusOK, "")

	// Повторный setup → 409.
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/setup", admin,
		gin.H{"pin": "654321"}), http.StatusConflict, CodePinAlreadySet)

	// verify неверного → 401 PIN_INVALID.
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/verify", admin,
		gin.H{"pin": "000000"}), http.StatusUnauthorized, CodePinInvalid)
	// verify верного → 200.
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/verify", admin,
		gin.H{"pin": "123456"}), http.StatusOK, "")
}

func TestPINFormatValidation(t *testing.T) {
	rg := newRig(t, newFakeStore())
	admin := rg.token(t, auth.RoleAdmin)
	for name, pin := range map[string]string{
		"короткий": "12345", "длинный": "1234567", "буквы": "12a456",
		"пустой": "", "с пробелом": "123 45",
	} {
		w := rg.do(t, http.MethodPost, "/api/emma/pin/setup", admin, gin.H{"pin": pin})
		if w.Code != http.StatusBadRequest {
			t.Errorf("setup %s (%q): %d, ждали 400", name, pin, w.Code)
		}
		w = rg.do(t, http.MethodPost, "/api/emma/pin/verify", admin, gin.H{"pin": pin})
		if w.Code != http.StatusBadRequest {
			t.Errorf("verify %s (%q): %d, ждали 400", name, pin, w.Code)
		}
	}
	// Кривой формат не инкрементит счётчик брутфорса — это 400, не промах.
	setupPIN(t, rg, admin, "123456")
	for i := 0; i < 10; i++ {
		rg.do(t, http.MethodPost, "/api/emma/pin/verify", admin, gin.H{"pin": "abc"})
	}
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/verify", admin,
		gin.H{"pin": "123456"}), http.StatusOK, "")
}

// --- брутфорс (критерий приёмки 4; реальные TTL — интеграционный тест) ---

func TestBruteForceLock(t *testing.T) {
	rg := newRig(t, newFakeStore())
	admin := rg.token(t, auth.RoleAdmin)
	setupPIN(t, rg, admin, "123456")
	wantStatus(t, rg.do(t, http.MethodDelete, "/api/emma/pin/session", admin, nil),
		http.StatusOK, "")

	for i := 0; i < MaxFails; i++ {
		wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/verify", admin,
			gin.H{"pin": "000000"}), http.StatusUnauthorized, CodePinInvalid)
	}
	// Шестая попытка — 429 с retry_after, даже с ВЕРНЫМ PIN (не проверяется).
	m := wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/verify", admin,
		gin.H{"pin": "123456"}), http.StatusTooManyRequests, CodePinLocked)
	if _, ok := m["retry_after"].(float64); !ok {
		t.Fatalf("429 без retry_after: %v", m)
	}
	// Сессия при этом не открылась.
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/status", admin, nil),
		http.StatusUnauthorized, CodePinRequired)

	// Окно истекло (в фейке — сброс счётчика) → верный PIN проходит.
	if err := rg.store.ResetFails(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/verify", admin,
		gin.H{"pin": "123456"}), http.StatusOK, "")
}

func TestSuccessResetsFailCounter(t *testing.T) {
	rg := newRig(t, newFakeStore())
	admin := rg.token(t, auth.RoleAdmin)
	setupPIN(t, rg, admin, "123456")

	// 4 промаха (до порога), успех — счётчик обнулён, снова 4 промаха без 429.
	for i := 0; i < MaxFails-1; i++ {
		rg.do(t, http.MethodPost, "/api/emma/pin/verify", admin, gin.H{"pin": "000000"})
	}
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/verify", admin,
		gin.H{"pin": "123456"}), http.StatusOK, "")
	for i := 0; i < MaxFails-1; i++ {
		wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/verify", admin,
			gin.H{"pin": "000000"}), http.StatusUnauthorized, CodePinInvalid)
	}
}

// --- сессия (критерий приёмки 5) ---

func TestSessionLifecycle(t *testing.T) {
	store := newFakeStore()
	rg := newRig(t, store)
	admin := rg.token(t, auth.RoleAdmin)
	setupPIN(t, rg, admin, "123456")

	// Запросы к защищённой группе проходят и продлевают TTL (sliding).
	before := store.touches
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/status", admin, nil),
		http.StatusOK, "")
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/status", admin, nil),
		http.StatusOK, "")
	if store.touches != before+2 {
		t.Errorf("touches = %d, ждали %d (каждый запрос продлевает)", store.touches, before+2)
	}

	// pin/status видит активную сессию.
	m := wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/pin/status", admin, nil),
		http.StatusOK, "")
	if !m["session_active"].(bool) {
		t.Fatal("session_active=false при живой сессии")
	}

	// Выход: DELETE session → следующий запрос 401 PIN_REQUIRED.
	wantStatus(t, rg.do(t, http.MethodDelete, "/api/emma/pin/session", admin, nil),
		http.StatusOK, "")
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/status", admin, nil),
		http.StatusUnauthorized, CodePinRequired)
}

// --- fail-closed (критерий приёмки 6) ---

func TestFailClosedOnRedisDown(t *testing.T) {
	rg := newRig(t, brokenStore{})
	admin := rg.token(t, auth.RoleAdmin)

	// verify → 503, НЕ пускает.
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/verify", admin,
		gin.H{"pin": "123456"}), http.StatusServiceUnavailable, CodePinUnavailable)
	// middleware защищённой группы → 503.
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/status", admin, nil),
		http.StatusServiceUnavailable, CodePinUnavailable)
	// pin/status и выход — тоже закрыты (единая дисциплина контура).
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/pin/status", admin, nil),
		http.StatusServiceUnavailable, CodePinUnavailable)
	wantStatus(t, rg.do(t, http.MethodDelete, "/api/emma/pin/session", admin, nil),
		http.StatusServiceUnavailable, CodePinUnavailable)
}

// --- pin/change (критерий приёмки 7) ---

func TestChangePIN(t *testing.T) {
	rg := newRig(t, newFakeStore())
	admin := rg.token(t, auth.RoleAdmin)
	setupPIN(t, rg, admin, "123456")

	// По неверному старому → 401.
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/change", admin,
		gin.H{"old_pin": "000000", "new_pin": "222222"}),
		http.StatusUnauthorized, CodePinInvalid)

	// По верному старому → 200; активная сессия НЕ рвётся.
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/change", admin,
		gin.H{"old_pin": "123456", "new_pin": "222222"}), http.StatusOK, "")
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/status", admin, nil),
		http.StatusOK, "")

	// Старый PIN больше не работает, новый — работает.
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/verify", admin,
		gin.H{"pin": "123456"}), http.StatusUnauthorized, CodePinInvalid)
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/verify", admin,
		gin.H{"pin": "222222"}), http.StatusOK, "")

	// Кривой формат нового → 400.
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/change", admin,
		gin.H{"old_pin": "222222", "new_pin": "22"}),
		http.StatusBadRequest, codeValidation)
}

// --- reset (критерий приёмки 8: ядро cmd/reset-emma-pin) ---

func TestReset(t *testing.T) {
	store := newFakeStore()
	rg := newRig(t, store)
	admin := rg.token(t, auth.RoleAdmin)
	setupPIN(t, rg, admin, "123456")

	purged, err := Reset(context.Background(), rg.settings, store)
	if err != nil {
		t.Fatal(err)
	}
	if purged == 0 {
		t.Error("Reset не удалил ни одного redis-ключа (сессия была открыта)")
	}

	// После сброса: pin_set=false, сессии нет, bootstrap открыт заново.
	m := wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/pin/status", admin, nil),
		http.StatusOK, "")
	if m["pin_set"].(bool) || m["session_active"].(bool) {
		t.Fatalf("после reset: %v", m)
	}
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/pin/setup", admin,
		gin.H{"pin": "999999"}), http.StatusOK, "")
}

// --- GET /api/emma/status (задача 6, ТЗ §2.3) ---

func TestStatusFactsOnly(t *testing.T) {
	rg := newRig(t, newFakeStore())
	admin := rg.token(t, auth.RoleAdmin)
	setupPIN(t, rg, admin, "123456")

	m := wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/status", admin, nil),
		http.StatusOK, "")
	if m["telegram_token_set"] != true || m["anthropic_key_set"] != true ||
		m["model"] != "claude-sonnet-5" || m["webhook_url_set"] != false {
		t.Fatalf("статус: %v", m)
	}
	// Только факты: ни одного поля со значением токена/ключа/URL.
	for key := range m {
		switch key {
		case "telegram_token_set", "anthropic_key_set", "model", "webhook_url_set":
		default:
			t.Errorf("лишнее поле в статусе: %s (ТЗ §2.3 — значения не выводить)", key)
		}
	}
}
