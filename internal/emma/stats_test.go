// Тесты EP-06 (вкладка 6): contract-тест GET /stats (структура ответа —
// по ней пишет фронт EP-07), стоимость = токены × константы, журнал
// ошибок (фильтр/пагинация/валидация), настройка алертов (400 на мусор,
// 400/502 у тест-алерта), fail-open Notifier при недоступном Redis.
package emma

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
	"github.com/interfin/interfin-ai-crm/internal/settings"
)

// memStatsRepo — repo.EmmaStatsRepo с канированными ответами и записью
// аргументов (проверка границ периодов и окна 24 ч).
type memStatsRepo struct {
	mu    sync.Mutex
	stats repo.EmmaStats
	dlg   repo.EmmaDialogStats
	errs  []models.EmmaEvent
	total int64

	statsFrom, statsTo    time.Time
	dlgActiveSince        time.Time
	lastKind              string
	lastPage              int
	errsFrom              time.Time
	statsCalls, errsCalls int
}

func (m *memStatsRepo) Stats(_ context.Context, from, to time.Time) (*repo.EmmaStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statsFrom, m.statsTo, m.statsCalls = from, to, m.statsCalls+1
	cp := m.stats
	return &cp, nil
}

func (m *memStatsRepo) ListErrors(_ context.Context, kind string, from, _ time.Time, page int) ([]models.EmmaEvent, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastKind, m.lastPage, m.errsFrom, m.errsCalls = kind, page, from, m.errsCalls+1
	return append([]models.EmmaEvent(nil), m.errs...), m.total, nil
}

func (m *memStatsRepo) DialogStats(_ context.Context, _, _, activeSince time.Time) (*repo.EmmaDialogStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dlgActiveSince = activeSince
	cp := m.dlg
	return &cp, nil
}

// memAlertSender — Sender: копит отправки, умеет отказывать (502-ветка).
type memAlertSender struct {
	mu   sync.Mutex
	sent []string
	err  error
}

func (m *memAlertSender) Send(_ int64, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, text)
	return nil
}

// statsRig — rig с PIN-сессией (все тесты ниже ходят за гейтами).
func statsRig(t *testing.T) (*rig, string) {
	t.Helper()
	rg := newRig(t, newFakeStore())
	admin := rg.token(t, "admin")
	setupPIN(t, rg, admin, "123456")
	return rg, admin
}

func fixedStats() repo.EmmaStats {
	id3 := int64(3)
	return repo.EmmaStats{
		Replies:       4,
		AvgResponseMs: 250,
		P95ResponseMs: 385,
		TokensIn:      1_000_000,
		TokensOut:     100_000,
		Handoffs:      2,
		FilesSent:     5,
		Files: []repo.EmmaFileSentCount{
			{SendFileID: &id3, Name: "Прайс 2026", Count: 3},
			{SendFileID: nil, Name: "(файл удалён)", Count: 2},
		},
		ErrorsByKind: map[string]int64{"llm_api": 2, "timeout": 1},
	}
}

// Contract-тест: структура GET /stats зафиксирована — фронт EP-07 пишет
// по ней; стоимость = токены × константы цен.
func TestEP06_StatsContract(t *testing.T) {
	rg, admin := statsRig(t)
	rg.stats.stats = fixedStats()
	rg.stats.dlg = repo.EmmaDialogStats{NewLeads: 10, ActiveDialogs: 7, MessagesIn: 100, MessagesOut: 90}

	m := wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/stats?period=week", admin, nil),
		http.StatusOK, "")

	for key, want := range map[string]float64{
		"replies": 4, "avg_response_ms": 250, "p95_response_ms": 385,
		"tokens_in": 1_000_000, "tokens_out": 100_000,
		"handoffs": 2, "files_sent_total": 5,
		"new_leads": 10, "active_dialogs_24h": 7,
		"messages_in": 100, "messages_out": 90,
	} {
		if got, _ := m[key].(float64); got != want {
			t.Errorf("%s = %v, ждали %v", key, m[key], want)
		}
	}
	// Стоимость: 1M входных × $3/1M + 0.1M выходных × $15/1M = $4.50.
	if got, _ := m["cost_usd_estimate"].(float64); math.Abs(got-4.5) > 1e-9 {
		t.Errorf("cost_usd_estimate = %v, ждали 4.5 (= токены × константы)", got)
	}
	if got, _ := m["pricing_note"].(string); got != pricingNote {
		t.Errorf("pricing_note = %q, ждали %q", got, pricingNote)
	}
	if got, _ := m["pricing_model"].(string); got != "claude-sonnet-5" {
		t.Errorf("pricing_model = %q", got)
	}
	if got, _ := m["period"].(string); got != "week" {
		t.Errorf("period = %q", got)
	}
	files, _ := m["files_sent"].([]any)
	if len(files) != 2 {
		t.Fatalf("files_sent: %v", m["files_sent"])
	}
	f0 := files[0].(map[string]any)
	if f0["name"] != "Прайс 2026" || f0["count"].(float64) != 3 || f0["send_file_id"].(float64) != 3 {
		t.Errorf("files_sent[0] = %v", f0)
	}
	if files[1].(map[string]any)["send_file_id"] != nil {
		t.Errorf("удалённый файл обязан отдавать send_file_id=null: %v", files[1])
	}
	errsMap, _ := m["errors"].(map[string]any)
	if errsMap["llm_api"].(float64) != 2 || errsMap["timeout"].(float64) != 1 {
		t.Errorf("errors = %v", errsMap)
	}

	// Границы: week ≈ [now-7d, now); активные диалоги — всегда 24 ч.
	now := time.Now().UTC()
	if d := now.Sub(rg.stats.statsFrom); d < 7*24*time.Hour-time.Minute || d > 7*24*time.Hour+time.Minute {
		t.Errorf("from при period=week: %v (дельта %v)", rg.stats.statsFrom, d)
	}
	if d := now.Sub(rg.stats.dlgActiveSince); d < 24*time.Hour-time.Minute || d > 24*time.Hour+time.Minute {
		t.Errorf("activeSince обязан быть now-24h независимо от периода: %v", rg.stats.dlgActiveSince)
	}
}

// period=all — from нулевой (вся история), мусорный period → 400.
func TestEP06_StatsPeriods(t *testing.T) {
	rg, admin := statsRig(t)

	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/stats?period=all", admin, nil),
		http.StatusOK, "")
	if !rg.stats.statsFrom.IsZero() {
		t.Errorf("period=all: from = %v, ждали нулевое время", rg.stats.statsFrom)
	}
	// Дефолт — day.
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/stats", admin, nil), http.StatusOK, "")
	if d := time.Since(rg.stats.statsFrom); d > 25*time.Hour || d < 23*time.Hour {
		t.Errorf("дефолтный период не day: from=%v", rg.stats.statsFrom)
	}
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/stats?period=year", admin, nil),
		http.StatusBadRequest, "ERR_VALIDATION")
}

// Журнал ошибок: контракт ответа, фильтр и страница доезжают до repo,
// валидация type/page.
func TestEP06_StatsErrors(t *testing.T) {
	rg, admin := statsRig(t)
	kind, detail, leadID := models.EmmaErrLLMAPI, "api 500", int64(7)
	rg.stats.errs = []models.EmmaEvent{{
		ID: 42, EventType: models.EmmaEventError, ErrorKind: &kind,
		Detail: &detail, LeadID: &leadID, CreatedAt: time.Now(),
	}}
	rg.stats.total = 120

	m := wantStatus(t, rg.do(t, http.MethodGet,
		"/api/emma/stats/errors?type=llm_api&page=2", admin, nil), http.StatusOK, "")
	if rg.stats.lastKind != "llm_api" || rg.stats.lastPage != 2 {
		t.Errorf("repo получил kind=%q page=%d", rg.stats.lastKind, rg.stats.lastPage)
	}
	if m["total"].(float64) != 120 || m["page"].(float64) != 2 || m["limit"].(float64) != 50 {
		t.Errorf("пагинация: %v", m)
	}
	items := m["errors"].([]any)
	it := items[0].(map[string]any)
	if it["error_kind"] != "llm_api" || it["detail"] != "api 500" || it["lead_id"].(float64) != 7 {
		t.Errorf("элемент журнала: %v", it)
	}

	// Дефолты: без параметров — все виды, страница 1, период all.
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/stats/errors", admin, nil), http.StatusOK, "")
	if rg.stats.lastKind != "" || rg.stats.lastPage != 1 || !rg.stats.errsFrom.IsZero() {
		t.Errorf("дефолты: kind=%q page=%d from=%v", rg.stats.lastKind, rg.stats.lastPage, rg.stats.errsFrom)
	}

	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/stats/errors?type=bogus", admin, nil),
		http.StatusBadRequest, "ERR_VALIDATION")
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/stats/errors?page=abc", admin, nil),
		http.StatusBadRequest, "ERR_VALIDATION")
	wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/stats/errors?page=0", admin, nil),
		http.StatusBadRequest, "ERR_VALIDATION")
}

// Критерий приёмки: PATCH /alerts с мусором («abc») → 400; валидный
// отрицательный id группы сохраняется; пусто = выключить.
func TestEP06_AlertsSettings(t *testing.T) {
	rg, admin := statsRig(t)

	m := wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/alerts", admin, nil), http.StatusOK, "")
	if m["enabled"].(bool) || m["chat_id"].(string) != "" {
		t.Errorf("до настройки: %v", m)
	}

	wantStatus(t, rg.do(t, http.MethodPatch, "/api/emma/alerts", admin,
		gin.H{"chat_id": "abc"}), http.StatusBadRequest, "ERR_VALIDATION")
	wantStatus(t, rg.do(t, http.MethodPatch, "/api/emma/alerts", admin,
		gin.H{}), http.StatusBadRequest, "ERR_VALIDATION")

	m = wantStatus(t, rg.do(t, http.MethodPatch, "/api/emma/alerts", admin,
		gin.H{"chat_id": "-100200300400"}), http.StatusOK, "")
	if !m["enabled"].(bool) || m["chat_id"].(string) != "-100200300400" {
		t.Errorf("после сохранения: %v", m)
	}
	if got := rg.settings.String(context.Background(), settings.KeyAlertChatID); got != "-100200300400" {
		t.Errorf("settings: %q", got)
	}

	m = wantStatus(t, rg.do(t, http.MethodPatch, "/api/emma/alerts", admin,
		gin.H{"chat_id": ""}), http.StatusOK, "")
	if m["enabled"].(bool) {
		t.Errorf("пустой chat_id обязан выключать алерты: %v", m)
	}
}

// Критерий приёмки: POST /alerts/test при пустом chat_id → 400, при
// отказе Telegram → 502; при успехе сообщение — «✅ Тестовый алерт…».
func TestEP06_AlertsTest(t *testing.T) {
	rg, admin := statsRig(t)

	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/alerts/test", admin, nil),
		http.StatusBadRequest, "ERR_VALIDATION")

	wantStatus(t, rg.do(t, http.MethodPatch, "/api/emma/alerts", admin,
		gin.H{"chat_id": "555"}), http.StatusOK, "")

	rg.alertSnd.err = errors.New("telegram: 403 bot was kicked")
	wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/alerts/test", admin, nil),
		http.StatusBadGateway, "ERR_TELEGRAM")

	rg.alertSnd.err = nil
	m := wantStatus(t, rg.do(t, http.MethodPost, "/api/emma/alerts/test", admin, nil),
		http.StatusOK, "")
	if !m["sent"].(bool) {
		t.Errorf("ответ: %v", m)
	}
	if len(rg.alertSnd.sent) != 1 || rg.alertSnd.sent[0] != "✅ Тестовый алерт панели Эммы" {
		t.Errorf("отправлено: %v", rg.alertSnd.sent)
	}
}

// Критерий приёмки (fail-open): Redis недоступен → алертов нет, методы
// Notifier не паникуют и не блокируют вызывающего (Эмма отвечает как
// обычно — recordEvent в processor best-effort; контраст с fail-closed
// PIN задокументирован в alerts.go).
func TestEP06_NotifierFailOpenWithoutRedis(t *testing.T) {
	// 127.0.0.1:1 — заведомо закрытый порт; таймауты короткие, чтобы тест
	// не ждал дефолтные 5 секунд на каждый вызов.
	dead := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1,
	})
	defer dead.Close()

	memRepo := &memSettingsRepo{values: map[string]string{
		settings.KeyAlertChatID: "555",
	}}
	snd := &memAlertSender{}
	n := NewNotifier(dead, settings.New(memRepo), snd,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	for i := 0; i < 4; i++ {
		n.OnError(ctx, models.EmmaErrLLMAPI, "api 500")
	}
	n.OnError(ctx, models.EmmaErrKBIndex, "scan.pdf: нет текстового слоя")
	n.OnSuccess(ctx)

	if len(snd.sent) != 0 {
		t.Errorf("без Redis алертов быть не может (fail-open): %v", snd.sent)
	}
}

// Формат алерта — ТЗ §3: «⚠️ Эмма: <тип>\nВремя: <UTC>\nПоследняя ошибка: …».
func TestEP06_AlertTextFormat(t *testing.T) {
	at := time.Date(2026, 7, 12, 10, 30, 0, 0, time.UTC)
	got := alertText(models.EmmaErrKBIndex, "scan.pdf: нет текстового слоя", at)
	want := "⚠️ Эмма: ошибка индексации базы знаний\nВремя: 2026-07-12 10:30:00 UTC\nПоследняя ошибка: scan.pdf: нет текстового слоя"
	if got != want {
		t.Errorf("алерт:\n%q\nждали:\n%q", got, want)
	}
	if !strings.Contains(alertText(models.EmmaErrLLMAPI, "x", at), "3 ошибки Claude API подряд") {
		t.Error("заголовок llm_api")
	}
}
