// Контрактные тесты EP-02: /api/emma/prompt* на риге EP-01 (JWT → admin →
// PIN-сессия). Репозиторий версий — in-memory фейк с семантикой боевого
// (одна активная, строки не мутируются); лимит токенов — боевой
// settings.Service поверх фейкового репозитория (SetString — часть критерия).
package emma

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
	"github.com/interfin/interfin-ai-crm/internal/settings"
	"github.com/interfin/interfin-ai-crm/internal/worker"
)

// memPromptRepo — in-memory repo.EmmaPromptRepo: та же дисциплина, что у
// боевого (CreateVersion снимает is_current со старой, превью — 100 рун).
type memPromptRepo struct {
	mu       sync.Mutex
	versions []models.EmmaPromptVersion
}

func newMemPromptRepo() *memPromptRepo { return &memPromptRepo{} }

func (m *memPromptRepo) GetCurrent(context.Context) (*models.EmmaPromptVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.versions {
		if m.versions[i].IsCurrent {
			cp := m.versions[i]
			return &cp, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (m *memPromptRepo) CreateVersion(_ context.Context, v *models.EmmaPromptVersion) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.versions {
		m.versions[i].IsCurrent = false
	}
	v.ID = int64(len(m.versions) + 1)
	v.IsCurrent = true
	v.CreatedAt = time.Now()
	m.versions = append(m.versions, *v)
	return nil
}

func (m *memPromptRepo) History(_ context.Context, page, perPage int) ([]repo.EmmaPromptHistoryItem, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	items := make([]repo.EmmaPromptHistoryItem, 0, len(m.versions))
	for i := len(m.versions) - 1; i >= 0; i-- { // новые → старые
		v := m.versions[i]
		preview := v.SystemPrompt
		if r := []rune(preview); len(r) > 100 { // руны, не байты — как LEFT()
			preview = string(r[:100])
		}
		items = append(items, repo.EmmaPromptHistoryItem{
			ID: v.ID, CreatedAt: v.CreatedAt, CreatedBy: v.CreatedBy,
			Preview: preview, Style: v.Style,
		})
	}
	total := int64(len(items))
	lo := (page - 1) * perPage
	if lo >= len(items) {
		return nil, total, nil
	}
	hi := lo + perPage
	if hi > len(items) {
		hi = len(items)
	}
	return items[lo:hi], total, nil
}

func (m *memPromptRepo) GetByID(_ context.Context, id int64) (*models.EmmaPromptVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id < 1 || id > int64(len(m.versions)) {
		return nil, repo.ErrNotFound
	}
	cp := m.versions[id-1]
	return &cp, nil
}

// promptRig — риг с открытой PIN-сессией (общая точка тестов EP-02).
func promptRig(t *testing.T) (*rig, string) {
	t.Helper()
	rg := newRig(t, newFakeStore())
	admin := rg.token(t, auth.RoleAdmin)
	setupPIN(t, rg, admin, "123456")
	return rg, admin
}

// TestPromptAdminWithoutPIN401 — критерий приёмки: admin без PIN-сессии
// получает 401 PIN_REQUIRED на КАЖДОЙ ручке prompt (manager 403 и запрос
// без токена покрывают общие гейт-тесты по emmaPaths).
func TestPromptAdminWithoutPIN401(t *testing.T) {
	rg := newRig(t, newFakeStore())
	admin := rg.token(t, auth.RoleAdmin)
	for _, p := range emmaPaths {
		if !strings.Contains(p.path, "/prompt") {
			continue
		}
		wantStatus(t, rg.do(t, p.method, p.path, admin, nil),
			http.StatusUnauthorized, CodePinRequired)
	}
}

// TestPromptGetEmptyTable404 — БД без сида 0021: редактировать нечего, 404
// (Эмма при этом живёт на константе — зона воркера, не ручки).
func TestPromptGetEmptyTable404(t *testing.T) {
	rg, admin := promptRig(t)
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/prompt", admin, nil),
		http.StatusNotFound, codeNotFound)
}

// TestPromptPutCreatesVersionAndGetReturnsIt — PUT создаёт активную версию,
// тело ответа = телу GET, token_estimate считается метрикой воркера.
func TestPromptPutCreatesVersionAndGetReturnsIt(t *testing.T) {
	rg, admin := promptRig(t)

	text := "Ты — Эмма. Отвечай коротко."
	m := wantStatus(t, rg.do(t, http.MethodPut, "/api/emma/prompt", admin, gin.H{
		"system_prompt":    text,
		"forbidden_topics": []string{"политика", " ", "религия"},
		"style":            "formal",
	}), http.StatusOK, "")

	if m["version_id"].(float64) != 1 || m["system_prompt"] != text || m["style"] != "formal" {
		t.Fatalf("тело PUT: %v", m)
	}
	if got := m["token_estimate"].(float64); int(got) != worker.EstimateTokens(text) {
		t.Errorf("token_estimate=%v, ждали метрику воркера %d", got, worker.EstimateTokens(text))
	}
	if m["token_limit"].(float64) != 1200 {
		t.Errorf("token_limit=%v, ждали дефолт 1200", m["token_limit"])
	}
	topics := m["forbidden_topics"].([]any)
	if len(topics) != 2 || topics[0] != "политика" || topics[1] != "религия" {
		t.Errorf("пустые теги не отброшены / темы кривые: %v", topics)
	}

	got := wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/prompt", admin, nil),
		http.StatusOK, "")
	if got["version_id"].(float64) != 1 || got["system_prompt"] != text {
		t.Fatalf("GET после PUT: %v", got)
	}

	// Вторая правка — новая версия, в репозитории ровно одна активная.
	wantStatus(t, rg.do(t, http.MethodPut, "/api/emma/prompt", admin, gin.H{
		"system_prompt": "Версия два.", "forbidden_topics": []string{}, "style": "neutral",
	}), http.StatusOK, "")
	cur, err := rg.prompts.GetCurrent(context.Background())
	if err != nil || cur.ID != 2 {
		t.Fatalf("активная версия: %+v, %v", cur, err)
	}
	if by := cur.CreatedBy; by == nil || *by != 1 {
		t.Errorf("created_by=%v, ждали sub=1", by)
	}
}

// TestPromptValidation — 400 на пустой текст, стиль вне enum, кривую
// страницу; 404 на нечисловой и несуществующий vid.
func TestPromptValidation(t *testing.T) {
	rg, admin := promptRig(t)
	for name, body := range map[string]gin.H{
		"пустой текст":   {"system_prompt": "   ", "style": "neutral"},
		"стиль вне enum": {"system_prompt": "Текст.", "style": "sarcastic"},
		"стиль не задан": {"system_prompt": "Текст."},
		"кривой тип тем": {"system_prompt": "Текст.", "style": "neutral", "forbidden_topics": "политика"},
	} {
		w := rg.do(t, http.MethodPut, "/api/emma/prompt", admin, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("PUT %s: %d, ждали 400; тело: %s", name, w.Code, w.Body.String())
		}
	}
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/prompt/history?page=0", admin, nil),
		http.StatusBadRequest, codeValidation)
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/prompt/history/abc", admin, nil),
		http.StatusNotFound, codeNotFound)
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/prompt/history/999", admin, nil),
		http.StatusNotFound, codeNotFound)
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/prompt/history/999/restore", admin, nil),
		http.StatusNotFound, codeNotFound)
}

// TestPromptTooLong — критерий приёмки: текст длиннее лимита → 400
// PROMPT_TOO_LONG с estimate и limit; после SetString лимита 2000 тот же
// текст проходит. 3000 кириллических символов = 6000 байт = 1500 токенов
// метрикой len/4 — больше дефолтных 1200, меньше 2000.
func TestPromptTooLong(t *testing.T) {
	rg, admin := promptRig(t)
	long := strings.Repeat("ф", 3000)

	m := wantStatus(t, rg.do(t, http.MethodPut, "/api/emma/prompt", admin, gin.H{
		"system_prompt": long, "style": "neutral",
	}), http.StatusBadRequest, CodePromptTooLong)
	if m["estimate"].(float64) != 1500 || m["limit"].(float64) != 1200 {
		t.Fatalf("estimate/limit: %v", m)
	}

	// Лимит поднят с сервера (SetString — ТЗ §3: меняется только так).
	if err := rg.settings.SetString(context.Background(),
		settings.KeyPromptTokenLimit, "2000"); err != nil {
		t.Fatal(err)
	}
	m = wantStatus(t, rg.do(t, http.MethodPut, "/api/emma/prompt", admin, gin.H{
		"system_prompt": long, "style": "neutral",
	}), http.StatusOK, "")
	if m["token_limit"].(float64) != 2000 {
		t.Errorf("token_limit=%v после SetString 2000", m["token_limit"])
	}
}

// TestPromptHistoryAndPreview — история новые → старые, превью 100 рун
// (кириллица не рвётся), total/page на месте.
func TestPromptHistoryAndPreview(t *testing.T) {
	rg, admin := promptRig(t)
	long := strings.Repeat("я", 150)
	for _, text := range []string{"Первая версия.", "Вторая версия.", long} {
		wantStatus(t, rg.do(t, http.MethodPut, "/api/emma/prompt", admin, gin.H{
			"system_prompt": text, "style": "neutral",
		}), http.StatusOK, "")
	}

	m := wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/prompt/history", admin, nil),
		http.StatusOK, "")
	if m["total"].(float64) != 3 || m["page"].(float64) != 1 {
		t.Fatalf("total/page: %v", m)
	}
	items := m["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("items: %d", len(items))
	}
	first := items[0].(map[string]any)
	if first["id"].(float64) != 3 {
		t.Errorf("история не с новых: %v", first)
	}
	preview := first["preview"].(string)
	if utf8.RuneCountInString(preview) != 100 || !utf8.ValidString(preview) ||
		preview != strings.Repeat("я", 100) {
		t.Errorf("превью не 100 рун / рвёт кириллицу: %q", preview)
	}
	if first["created_by"].(float64) != 1 {
		t.Errorf("created_by: %v", first["created_by"])
	}

	// Просмотр версии — полный текст, не превью.
	got := wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/prompt/history/3", admin, nil),
		http.StatusOK, "")
	if got["system_prompt"] != long || got["is_current"] != true {
		t.Errorf("полная версия: %v", got)
	}
}

// TestPromptRestore — критерий приёмки: restore версии N делает активной
// КОПИЮ N (текст, темы, стиль), прежняя активная остаётся в истории.
func TestPromptRestore(t *testing.T) {
	rg, admin := promptRig(t)
	wantStatus(t, rg.do(t, http.MethodPut, "/api/emma/prompt", admin, gin.H{
		"system_prompt":    "Версия один.",
		"forbidden_topics": []string{"политика"},
		"style":            "expert",
	}), http.StatusOK, "")
	wantStatus(t, rg.do(t, http.MethodPut, "/api/emma/prompt", admin, gin.H{
		"system_prompt": "Версия два.", "style": "neutral",
	}), http.StatusOK, "")

	m := wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/prompt/history/1/restore", admin, nil),
		http.StatusOK, "")
	// Активная — НОВАЯ версия (id 3) со всеми тремя полями версии 1.
	if m["version_id"].(float64) != 3 || m["system_prompt"] != "Версия один." ||
		m["style"] != "expert" {
		t.Fatalf("restore вернул: %v", m)
	}
	topics := m["forbidden_topics"].([]any)
	if len(topics) != 1 || topics[0] != "политика" {
		t.Fatalf("темы не восстановлены: %v", topics)
	}

	// GET видит восстановленную; прежняя активная (2) — в истории, не активна.
	got := wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/prompt", admin, nil),
		http.StatusOK, "")
	if got["version_id"].(float64) != 3 {
		t.Fatalf("после restore активна %v, ждали 3", got["version_id"])
	}
	hist := wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/prompt/history", admin, nil),
		http.StatusOK, "")
	if hist["total"].(float64) != 3 {
		t.Fatalf("история после restore: %v", hist)
	}
	v2 := wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/prompt/history/2", admin, nil),
		http.StatusOK, "")
	if v2["is_current"] != false {
		t.Error("прежняя активная версия не ушла в историю")
	}
}
