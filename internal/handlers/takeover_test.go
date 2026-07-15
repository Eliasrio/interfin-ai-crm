// Контрактные тесты M13 на общем риге api_contract_test.go:
// PATCH /api/leads/:id/mode, автопилот hybrid в чате M12 и /api/settings.
package handlers

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/settings"
)

func (rig *apiRig) modeEvents() []events.Event {
	var out []events.Event
	for _, ev := range rig.pub.events {
		if ev.Type == events.TypeDialogMode {
			out = append(out, ev)
		}
	}
	return out
}

// --- PATCH /api/leads/:id/mode ---

func TestPatchMode_TakeAndReturn(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	token := rig.token(t, auth.RoleManager) // sub="1"

	// «Взять в работу»: mode=human, taken_by из JWT, пауза снята.
	m := wantStatus(t, rig.do(t, http.MethodPatch, "/api/leads/1/mode", token,
		gin.H{"mode": "human"}), http.StatusOK, "")
	lead := m["lead"].(map[string]any)
	if lead["dialog_mode"] != "human" || lead["taken_by"].(float64) != 1 {
		t.Fatalf("после take: %v", lead)
	}
	if _, has := lead["bot_silenced_until"]; has {
		t.Fatal("переход в human обязан снимать паузу автопилота")
	}
	evs := rig.modeEvents()
	if len(evs) != 1 || evs[0].Mode != "human" || evs[0].TakenBy == nil || *evs[0].TakenBy != 1 {
		t.Fatalf("событие dialog_mode: %+v", evs)
	}

	// Идемпотентный повтор — 200 (double click).
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/leads/1/mode", token,
		gin.H{"mode": "human"}), http.StatusOK, "")

	// «Вернуть Эмме»: mode=bot, владелец снят.
	m = wantStatus(t, rig.do(t, http.MethodPatch, "/api/leads/1/mode", token,
		gin.H{"mode": "bot"}), http.StatusOK, "")
	lead = m["lead"].(map[string]any)
	if lead["dialog_mode"] != "bot" {
		t.Fatalf("после return: %v", lead)
	}
	if _, has := lead["taken_by"]; has {
		t.Fatal("возврат Эмме обязан снимать taken_by")
	}
	rig.store.mu.Lock()
	got := rig.store.leads[1]
	if got.DialogMode != models.DialogModeBot || got.TakenBy != nil || got.BotSilencedUntil != nil {
		t.Fatalf("состояние лида после возврата: %+v", got)
	}
	rig.store.mu.Unlock()
}

// TestPatchMode_HumanClearsPause — взятие в работу поверх активной паузы:
// пауза снимается (человек главнее автопилота, состояние не смешивается).
func TestPatchMode_HumanClearsPause(t *testing.T) {
	lead := testLead(1, 2)
	until := time.Now().Add(20 * time.Minute)
	lead.BotSilencedUntil = &until
	rig := newAPIRig(t, lead)

	wantStatus(t, rig.do(t, http.MethodPatch, "/api/leads/1/mode",
		rig.token(t, auth.RoleManager), gin.H{"mode": "human"}), http.StatusOK, "")
	rig.store.mu.Lock()
	defer rig.store.mu.Unlock()
	if rig.store.leads[1].BotSilencedUntil != nil {
		t.Fatal("пауза обязана сняться при переходе в human")
	}
}

func TestPatchMode_Validation(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	token := rig.token(t, auth.RoleManager)

	for name, body := range map[string]any{
		"без mode":      gin.H{},
		"мусорный":      gin.H{"mode": "auto"},
		"не строка":     gin.H{"mode": 42},
		"пустая строка": gin.H{"mode": " "},
	} {
		w := rig.do(t, http.MethodPatch, "/api/leads/1/mode", token, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: статус %d, ждали 400; тело: %s", name, w.Code, w.Body.String())
		}
	}

	// Нет лида — 404; стёртый — тоже 404 (как остальные ручки M8).
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/leads/999/mode", token,
		gin.H{"mode": "human"}), http.StatusNotFound, "ERR_NOT_FOUND")
	wantStatus(t, rig.do(t, http.MethodDelete, "/api/lgpd/leads/1/erase", token, nil),
		http.StatusOK, "")
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/leads/1/mode", token,
		gin.H{"mode": "human"}), http.StatusNotFound, "ERR_NOT_FOUND")
}

// --- автопилот hybrid: пауза после реплики менеджера (критерий 3) ---

func TestAutopilot_PauseAfterManagerReply(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	token := rig.token(t, auth.RoleManager)

	before := time.Now()
	wantStatus(t, rig.do(t, http.MethodPost, "/api/leads/1/messages", token,
		gin.H{"text": "отвечаю сам"}), http.StatusOK, "")

	rig.store.mu.Lock()
	until := rig.store.leads[1].BotSilencedUntil
	rig.store.mu.Unlock()
	if until == nil {
		t.Fatal("реплика менеджера при mode=bot обязана ставить паузу Эммы")
	}
	// Дефолт hybrid_pause_minutes = 30: NOW+30м (с минутой люфта на тест).
	want := before.Add(30 * time.Minute)
	if until.Before(want.Add(-time.Minute)) || until.After(want.Add(time.Minute)) {
		t.Errorf("пауза до %v, ожидали ≈ %v (30 мин)", until, want)
	}
	evs := rig.modeEvents()
	if len(evs) != 1 || evs[0].Mode != models.DialogModeBot || evs[0].SilencedUntil == nil {
		t.Fatalf("событие dialog_mode автопилота: %+v", evs)
	}
}

// TestAutopilot_RespectsSettings — интервал берётся из settings (PATCH
// admin), а не из констант.
func TestAutopilot_RespectsSettings(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/settings", rig.token(t, auth.RoleAdmin),
		gin.H{"takeover.hybrid_pause_minutes": 5}), http.StatusOK, "")

	before := time.Now()
	wantStatus(t, rig.do(t, http.MethodPost, "/api/leads/1/messages",
		rig.token(t, auth.RoleManager), gin.H{"text": "коротко"}), http.StatusOK, "")

	rig.store.mu.Lock()
	until := rig.store.leads[1].BotSilencedUntil
	rig.store.mu.Unlock()
	want := before.Add(5 * time.Minute)
	if until == nil || until.Before(want.Add(-time.Minute)) || until.After(want.Add(time.Minute)) {
		t.Errorf("пауза до %v, ожидали ≈ %v (5 мин из settings)", until, want)
	}
}

// TestAutopilot_HumanModeUntouched — в режиме human пауза НЕ трогается:
// Эмму глушит сам режим, лишнее событие dialog_mode не публикуется.
func TestAutopilot_HumanModeUntouched(t *testing.T) {
	lead := testLead(1, 2)
	lead.DialogMode = models.DialogModeHuman
	rig := newAPIRig(t, lead)

	wantStatus(t, rig.do(t, http.MethodPost, "/api/leads/1/messages",
		rig.token(t, auth.RoleManager), gin.H{"text": "веду диалог"}), http.StatusOK, "")

	rig.store.mu.Lock()
	until := rig.store.leads[1].BotSilencedUntil
	rig.store.mu.Unlock()
	if until != nil {
		t.Error("в режиме human пауза не ставится")
	}
	if evs := rig.modeEvents(); len(evs) != 0 {
		t.Errorf("событий dialog_mode %d, ожидали 0", len(evs))
	}
}

// TestAutopilot_InvoiceAlsoPauses — счёт из карточки — та же реплика
// менеджера: пауза ставится и после него.
func TestAutopilot_InvoiceAlsoPauses(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	wantStatus(t, rig.do(t, http.MethodPost, "/api/leads/1/invoice",
		rig.token(t, auth.RoleManager), gin.H{"amount": "2000", "asset": "USDT"}),
		http.StatusOK, "")

	rig.store.mu.Lock()
	defer rig.store.mu.Unlock()
	if rig.store.leads[1].BotSilencedUntil == nil {
		t.Fatal("счёт из карточки обязан ставить паузу Эммы (mode=bot)")
	}
}

// --- GET/PATCH /api/settings (критерий 6) ---

func TestSettings_GetDefaults(t *testing.T) {
	rig := newAPIRig(t)
	m := wantStatus(t, rig.do(t, http.MethodGet, "/api/settings",
		rig.token(t, auth.RoleManager), nil), http.StatusOK, "")
	s := m["settings"].(map[string]any)
	if s["takeover.hybrid_pause_minutes"].(float64) != 30 ||
		s["takeover.reminder_minutes"].(float64) != 10 ||
		s["takeover.pickup_minutes"].(float64) != 10 {
		t.Fatalf("дефолты: %v", s)
	}
}

func TestSettings_AdminPatch(t *testing.T) {
	rig := newAPIRig(t)
	admin := rig.token(t, auth.RoleAdmin)

	m := wantStatus(t, rig.do(t, http.MethodPatch, "/api/settings", admin,
		gin.H{"takeover.reminder_minutes": 5, "takeover.pickup_minutes": 15}),
		http.StatusOK, "")
	s := m["settings"].(map[string]any)
	if s["takeover.reminder_minutes"].(float64) != 5 || s["takeover.pickup_minutes"].(float64) != 15 {
		t.Fatalf("после PATCH: %v", s)
	}

	// Изменение видно и в GET (сквозь сервис/кэш).
	m = wantStatus(t, rig.do(t, http.MethodGet, "/api/settings", admin, nil), http.StatusOK, "")
	if m["settings"].(map[string]any)["takeover.reminder_minutes"].(float64) != 5 {
		t.Fatal("PATCH не доехал до GET")
	}
}

func TestSettings_ManagerForbidden(t *testing.T) {
	rig := newAPIRig(t)
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/settings", rig.token(t, auth.RoleManager),
		gin.H{"takeover.reminder_minutes": 5}), http.StatusForbidden, "ERR_FORBIDDEN")
}

// --- GET/PATCH /api/stages (названия этапов, 2026-07-15) ---

func TestStages_GetDefaultsAndRename(t *testing.T) {
	rig := newAPIRig(t)
	manager := rig.token(t, auth.RoleManager)
	admin := rig.token(t, auth.RoleAdmin)

	// Дефолты видит и manager.
	m := wantStatus(t, rig.do(t, http.MethodGet, "/api/stages", manager, nil), http.StatusOK, "")
	names := m["names"].(map[string]any)
	if names["1"] != "Серые лиды" || names["8"] != "Не удалось" {
		t.Fatalf("дефолтные названия: %v", names)
	}

	// Переименование admin'ом видно в GET (пробелы триммятся).
	m = wantStatus(t, rig.do(t, http.MethodPatch, "/api/stages", admin,
		gin.H{"1": "  Новые заявки ", "7": "Оплачен договор"}), http.StatusOK, "")
	names = m["names"].(map[string]any)
	if names["1"] != "Новые заявки" || names["7"] != "Оплачен договор" || names["2"] != "Живые лиды" {
		t.Fatalf("после PATCH: %v", names)
	}
	m = wantStatus(t, rig.do(t, http.MethodGet, "/api/stages", manager, nil), http.StatusOK, "")
	if m["names"].(map[string]any)["1"] != "Новые заявки" {
		t.Fatal("PATCH не доехал до GET")
	}
}

func TestStages_Validation(t *testing.T) {
	rig := newAPIRig(t)
	admin := rig.token(t, auth.RoleAdmin)

	// Manager не может переименовывать.
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/stages", rig.token(t, auth.RoleManager),
		gin.H{"1": "x"}), http.StatusForbidden, "ERR_FORBIDDEN")
	// Неизвестный этап.
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/stages", admin,
		gin.H{"9": "Лишний"}), http.StatusBadRequest, "ERR_UNKNOWN_KEY")
	// Пустое название; всё-или-ничего — валидный "2" не применяется.
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/stages", admin,
		gin.H{"2": "Горячие", "3": "   "}), http.StatusBadRequest, "ERR_VALIDATION")
	m := wantStatus(t, rig.do(t, http.MethodGet, "/api/stages", admin, nil), http.StatusOK, "")
	if m["names"].(map[string]any)["2"] != "Живые лиды" {
		t.Fatal("частичная запись при ошибке валидации")
	}
	// Слишком длинное название (61 символ кириллицы).
	long := strings.Repeat("ы", 61)
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/stages", admin,
		gin.H{"4": long}), http.StatusBadRequest, "ERR_VALIDATION")
}

// TestSettings_EmmaPanelKeysHidden — EP-01: служебный emma_panel.pin_hash
// (и остальные строковые ключи панели) не света через /api/settings —
// они управляются только внутренним контуром /api/emma/*.
func TestSettings_EmmaPanelKeysHidden(t *testing.T) {
	rig := newAPIRig(t)
	admin := rig.token(t, auth.RoleAdmin)

	// Даже записанный в БД pin_hash не появляется в GET.
	if err := rig.settings.SetString(context.Background(),
		settings.KeyPinHash, "$2a$12$hash"); err != nil {
		t.Fatal(err)
	}
	m := wantStatus(t, rig.do(t, http.MethodGet, "/api/settings", admin, nil),
		http.StatusOK, "")
	for key := range m["settings"].(map[string]any) {
		if strings.HasPrefix(key, "emma_panel.") {
			t.Errorf("GET /api/settings отдал ключ панели: %s", key)
		}
	}

	// PATCH с pin_hash → 400 ERR_UNKNOWN_KEY, значение не тронуто.
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/settings", admin,
		gin.H{"emma_panel.pin_hash": 123456}), http.StatusBadRequest, "ERR_UNKNOWN_KEY")
	if got := rig.settings.String(context.Background(), settings.KeyPinHash); got != "$2a$12$hash" {
		t.Errorf("PATCH дотянулся до pin_hash: %q", got)
	}
}

func TestSettings_PatchValidation(t *testing.T) {
	rig := newAPIRig(t)
	admin := rig.token(t, auth.RoleAdmin)

	for name, body := range map[string]any{
		"ноль":          gin.H{"takeover.reminder_minutes": 0},
		"отрицательное": gin.H{"takeover.reminder_minutes": -5},
		"строка-мусор":  gin.H{"takeover.reminder_minutes": "abc"},
		"дробное":       gin.H{"takeover.reminder_minutes": 2.5},
		"больше суток":  gin.H{"takeover.reminder_minutes": 1441},
		"пустое тело":   gin.H{},
	} {
		w := rig.do(t, http.MethodPatch, "/api/settings", admin, body)
		wantStatus(t, w, http.StatusBadRequest, "ERR_VALIDATION")
		_ = name
	}

	// Неизвестный ключ — отдельный код ERR_UNKNOWN_KEY (EP-01).
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/settings", admin,
		gin.H{"takeover.unknown": 10}), http.StatusBadRequest, "ERR_UNKNOWN_KEY")

	// Всё или ничего: валидный ключ в одном запросе с мусорным НЕ применяется.
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/settings", admin,
		gin.H{"takeover.pickup_minutes": 20, "takeover.reminder_minutes": -1}),
		http.StatusBadRequest, "ERR_VALIDATION")
	m := wantStatus(t, rig.do(t, http.MethodGet, "/api/settings", admin, nil), http.StatusOK, "")
	if m["settings"].(map[string]any)["takeover.pickup_minutes"].(float64) != 10 {
		t.Fatal("частичное применение PATCH при ошибке валидации")
	}
}
