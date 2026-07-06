package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// protectedRouter — маршрут, как его соберёт M8: Middleware + RequireRole.
func protectedRouter(ver *Verifier, roles ...string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/api", Middleware(ver))
	if len(roles) > 0 {
		grp.Use(RequireRole(roles...))
	}
	grp.GET("/leads", func(c *gin.Context) {
		claims, _ := ClaimsFrom(c)
		c.JSON(http.StatusOK, gin.H{"sub": claims.Subject, "role": claims.Role})
	})
	return r
}

func doGet(t *testing.T, r *gin.Engine, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/leads", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не {error,code}: %s", w.Body.String())
	}
	return body.Code
}

func TestMiddlewareValidToken(t *testing.T) {
	iss, ver := testPair(t, 15*time.Minute)
	token, _ := iss.Issue("42", RoleManager)

	w := doGet(t, protectedRouter(ver, RoleManager, RoleAdmin), "Bearer "+token)
	if w.Code != http.StatusOK {
		t.Fatalf("код %d, тело %s", w.Code, w.Body.String())
	}
}

// AQ²-2 (критерий приёмки M7): просроченный JWT → 401.
func TestMiddlewareExpiredToken401(t *testing.T) {
	iss, _ := testPair(t, -time.Minute)
	_, ver := testPair(t, 15*time.Minute)
	token, _ := iss.Issue("42", RoleManager)

	w := doGet(t, protectedRouter(ver, RoleManager), "Bearer "+token)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("код %d, ожидался 401", w.Code)
	}
	if code := errCode(t, w); code != "ERR_TOKEN_EXPIRED" {
		t.Fatalf("code %q, ожидался ERR_TOKEN_EXPIRED", code)
	}
}

// AQ²-2 (критерий приёмки M7): валидный токен с неверной ролью → 403.
func TestMiddlewareWrongRole403(t *testing.T) {
	iss, ver := testPair(t, 15*time.Minute)
	token, _ := iss.Issue("42", RoleManager) // manager ломится в admin-роут

	w := doGet(t, protectedRouter(ver, RoleAdmin), "Bearer "+token)
	if w.Code != http.StatusForbidden {
		t.Fatalf("код %d, ожидался 403", w.Code)
	}
	if code := errCode(t, w); code != "ERR_FORBIDDEN" {
		t.Fatalf("code %q, ожидался ERR_FORBIDDEN", code)
	}
}

func TestMiddlewareSystemRole(t *testing.T) {
	iss, ver := testPair(t, 15*time.Minute)
	sysToken, _ := iss.Issue("retention-cron", RoleSystem)

	// system проходит на internal-роут (§5.2)…
	if w := doGet(t, protectedRouter(ver, RoleSystem), "Bearer "+sysToken); w.Code != http.StatusOK {
		t.Fatalf("system на internal: код %d", w.Code)
	}
	// …и не проходит на менеджерский.
	if w := doGet(t, protectedRouter(ver, RoleManager, RoleAdmin), "Bearer "+sysToken); w.Code != http.StatusForbidden {
		t.Fatalf("system на менеджерском: код %d, ожидался 403", w.Code)
	}
}

func TestMiddlewareMissingOrMalformedHeader(t *testing.T) {
	iss, ver := testPair(t, 15*time.Minute)
	token, _ := iss.Issue("42", RoleManager)
	r := protectedRouter(ver, RoleManager)

	for name, header := range map[string]string{
		"нет заголовка": "",
		"без Bearer":    token,
		"Basic вместо":  "Basic dXNlcjpwYXNz",
		"битый токен":   "Bearer мусор",
	} {
		w := doGet(t, r, header)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: код %d, ожидался 401", name, w.Code)
		}
	}
}
