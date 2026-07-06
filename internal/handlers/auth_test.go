package handlers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// --- фейки репозиториев M7 ---

type fakeManagers struct {
	byID map[int64]*models.Manager
}

func (f *fakeManagers) Create(_ context.Context, m *models.Manager) error {
	f.byID[m.ID] = m
	return nil
}

func (f *fakeManagers) GetByEmail(_ context.Context, email string) (*models.Manager, error) {
	for _, m := range f.byID {
		if m.Email == email && m.Active {
			return m, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (f *fakeManagers) GetByID(_ context.Context, id int64) (*models.Manager, error) {
	m, ok := f.byID[id]
	if !ok || !m.Active {
		return nil, repo.ErrNotFound
	}
	return m, nil
}

type fakeTokens struct {
	byHash map[string]*models.RefreshToken
}

func (f *fakeTokens) Create(_ context.Context, rt *models.RefreshToken) error {
	f.byHash[rt.TokenHash] = rt
	return nil
}

func (f *fakeTokens) Consume(_ context.Context, hash string) (*models.RefreshToken, error) {
	rt, ok := f.byHash[hash]
	if !ok {
		return nil, repo.ErrNotFound
	}
	delete(f.byHash, hash)
	return rt, nil
}

func (f *fakeTokens) DeleteExpired(_ context.Context) error {
	for h, rt := range f.byHash {
		if rt.ExpiresAt.Before(time.Now()) {
			delete(f.byHash, h)
		}
	}
	return nil
}

// --- сборка тестового окружения ---

type authEnv struct {
	router   *gin.Engine
	managers *fakeManagers
	tokens   *fakeTokens
	issuer   *auth.Issuer
	verifier *auth.Verifier
	key      *rsa.PrivateKey
}

const (
	testEmail    = "manager@interfin.com"
	testPassword = "correct-horse-battery"
)

func newAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}

	env := &authEnv{
		managers: &fakeManagers{byID: map[int64]*models.Manager{
			1: {ID: 1, Email: testEmail, PasswordHash: hash,
				Role: auth.RoleManager, Active: true},
		}},
		tokens:   &fakeTokens{byHash: map[string]*models.RefreshToken{}},
		issuer:   auth.NewIssuer(key, 900*time.Second), // 15 мин (§5.1)
		verifier: auth.NewVerifier(&key.PublicKey),
		key:      key,
	}

	env.router = gin.New()
	NewAuth(AuthDeps{
		Managers:   env.managers,
		Tokens:     env.tokens,
		Issuer:     env.issuer,
		RefreshTTL: 7 * 24 * time.Hour, // 7 дней (§5.1)
		Log:        slog.New(slog.NewTextHandler(&strings.Builder{}, nil)),
	}).Register(env.router)
	return env
}

func (e *authEnv) login(t *testing.T, email, password string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"email":"` + email + `","password":"` + password + `"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func (e *authEnv) refresh(t *testing.T, cookie *http.Cookie, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func refreshCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == "refresh_token" {
			return c
		}
	}
	return nil
}

func tokenResponse(t *testing.T, w *httptest.ResponseRecorder) (accessToken string, expiresIn int) {
	t.Helper()
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело ответа: %s", w.Body.String())
	}
	return body.AccessToken, body.ExpiresIn
}

// --- login ---

// Задача M7-2: логин отдаёт { access_token, expires_in: 900 } + refresh cookie.
func TestLoginSuccess(t *testing.T) {
	env := newAuthEnv(t)
	w := env.login(t, testEmail, testPassword)
	if w.Code != http.StatusOK {
		t.Fatalf("код %d: %s", w.Code, w.Body.String())
	}

	access, expiresIn := tokenResponse(t, w)
	if expiresIn != 900 {
		t.Errorf("expires_in = %d, ожидалось 900 (§5.1)", expiresIn)
	}
	claims, err := env.verifier.Verify(access)
	if err != nil {
		t.Fatalf("access-токен не проходит верификацию: %v", err)
	}
	if claims.Subject != "1" || claims.Role != auth.RoleManager {
		t.Errorf("claims: sub=%q role=%q", claims.Subject, claims.Role)
	}

	c := refreshCookie(t, w)
	if c == nil {
		t.Fatal("refresh cookie не установлена")
	}
	// §5.1: HttpOnly; плюс Secure, SameSite=Strict и Path-scope /auth.
	if !c.HttpOnly || !c.Secure || c.Path != "/auth" || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("флаги cookie: HttpOnly=%v Secure=%v Path=%q SameSite=%v",
			c.HttpOnly, c.Secure, c.Path, c.SameSite)
	}
	if c.MaxAge != 7*24*3600 {
		t.Errorf("MaxAge = %d, ожидалось 604800 (7 дней, §5.1)", c.MaxAge)
	}

	// В хранилище — только хеш токена, не сам токен.
	if _, ok := env.tokens.byHash[c.Value]; ok {
		t.Error("refresh-токен лежит в хранилище открытым текстом")
	}
	if _, ok := env.tokens.byHash[auth.HashRefreshToken(c.Value)]; !ok {
		t.Error("хеш выданного refresh-токена не сохранён")
	}
}

func TestLoginRejects(t *testing.T) {
	env := newAuthEnv(t)
	cases := map[string]struct {
		email, password string
	}{
		"неверный пароль":   {testEmail, "wrong"},
		"неизвестный email": {"ghost@interfin.com", testPassword},
	}
	for name, tc := range cases {
		w := env.login(t, tc.email, tc.password)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: код %d, ожидался 401", name, w.Code)
		}
		if refreshCookie(t, w) != nil {
			t.Errorf("%s: выдана refresh cookie при отказе", name)
		}
	}
}

func TestLoginInactiveManager(t *testing.T) {
	env := newAuthEnv(t)
	env.managers.byID[1].Active = false
	if w := env.login(t, testEmail, testPassword); w.Code != http.StatusUnauthorized {
		t.Fatalf("деактивированный менеджер вошёл: код %d", w.Code)
	}
}

func TestLoginBadJSON(t *testing.T) {
	env := newAuthEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"email":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("код %d, ожидался 400", w.Code)
	}
}

// --- refresh ---

// Критерий приёмки M7: /auth/refresh работает при ИСТЁКШЕМ access token —
// авторизация только по refresh-cookie, Authorization не читается вовсе.
func TestRefreshWorksWithExpiredAccessToken(t *testing.T) {
	env := newAuthEnv(t)
	cookie := refreshCookie(t, env.login(t, testEmail, testPassword))

	// Просроченный access в Authorization — как сделает реальный фронт,
	// у которого access истёк, а страница не перезагружалась.
	expiredIssuer := auth.NewIssuer(env.key, -time.Minute)
	expiredAccess, err := expiredIssuer.Issue("1", auth.RoleManager)
	if err != nil {
		t.Fatal(err)
	}

	w := env.refresh(t, cookie, "Bearer "+expiredAccess)
	if w.Code != http.StatusOK {
		t.Fatalf("refresh при истёкшем access: код %d: %s", w.Code, w.Body.String())
	}
	access, expiresIn := tokenResponse(t, w)
	if expiresIn != 900 {
		t.Errorf("expires_in = %d", expiresIn)
	}
	if _, err := env.verifier.Verify(access); err != nil {
		t.Fatalf("новый access-токен невалиден: %v", err)
	}
}

// Ротация: предъявленный refresh гасится, старая cookie второй раз не работает.
func TestRefreshRotation(t *testing.T) {
	env := newAuthEnv(t)
	first := refreshCookie(t, env.login(t, testEmail, testPassword))

	w1 := env.refresh(t, first, "")
	if w1.Code != http.StatusOK {
		t.Fatalf("первый refresh: код %d", w1.Code)
	}
	second := refreshCookie(t, w1)
	if second == nil || second.Value == first.Value {
		t.Fatal("refresh не выдал новую cookie (ротация не работает)")
	}

	// Повтор старого токена → 401 (double-spend закрыт).
	if w := env.refresh(t, first, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("повтор погашенного refresh: код %d, ожидался 401", w.Code)
	}
	// Новый токен жив.
	if w := env.refresh(t, second, ""); w.Code != http.StatusOK {
		t.Fatalf("свежий refresh: код %d", w.Code)
	}
}

func TestRefreshMissingCookie(t *testing.T) {
	env := newAuthEnv(t)
	if w := env.refresh(t, nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("без cookie: код %d, ожидался 401", w.Code)
	}
}

func TestRefreshExpiredToken(t *testing.T) {
	env := newAuthEnv(t)
	cookie := refreshCookie(t, env.login(t, testEmail, testPassword))

	// Просрочиваем строку в хранилище (7 дней прошли).
	env.tokens.byHash[auth.HashRefreshToken(cookie.Value)].ExpiresAt =
		time.Now().Add(-time.Hour)

	w := env.refresh(t, cookie, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("просроченный refresh: код %d, ожидался 401", w.Code)
	}
	// Просроченная строка погашена, повтор тоже 401.
	if _, ok := env.tokens.byHash[auth.HashRefreshToken(cookie.Value)]; ok {
		t.Error("просроченный refresh-токен остался в хранилище")
	}
}

func TestRefreshDeactivatedManager(t *testing.T) {
	env := newAuthEnv(t)
	cookie := refreshCookie(t, env.login(t, testEmail, testPassword))
	env.managers.byID[1].Active = false

	if w := env.refresh(t, cookie, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("refresh деактивированного менеджера: код %d, ожидался 401", w.Code)
	}
}

func TestRefreshGarbageCookie(t *testing.T) {
	env := newAuthEnv(t)
	w := env.refresh(t, &http.Cookie{Name: "refresh_token", Value: "garbage-not-a-uuid"}, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("мусорная cookie: код %d, ожидался 401", w.Code)
	}
}
