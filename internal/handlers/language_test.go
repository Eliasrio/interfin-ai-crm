// Контрактные тесты M14 на общем риге api_contract_test.go:
// PATCH /api/leads/:id/language и language в leadJSON.
package handlers

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/events"
)

func (rig *apiRig) languageEvents() []events.Event {
	var out []events.Event
	for _, ev := range rig.pub.events {
		if ev.Type == events.TypeLeadLanguage {
			out = append(out, ev)
		}
	}
	return out
}

// --- PATCH /api/leads/:id/language ---

func TestPatchLanguage_ManagerChanges(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	token := rig.token(t, auth.RoleManager)

	m := wantStatus(t, rig.do(t, http.MethodPatch, "/api/leads/1/language", token,
		gin.H{"language": "es"}), http.StatusOK, "")
	lead := m["lead"].(map[string]any)
	if lead["language"] != "es" {
		t.Fatalf("после PATCH: %v", lead)
	}
	rig.store.mu.Lock()
	got := rig.store.leads[1].Language
	rig.store.mu.Unlock()
	if got == nil || *got != "es" {
		t.Fatalf("язык в хранилище: %v", got)
	}
	evs := rig.languageEvents()
	if len(evs) != 1 || evs[0].LeadID != 1 || evs[0].Language != "es" {
		t.Fatalf("событие lead_language: %+v", evs)
	}

	// Идемпотентный повтор — 200 (double click, вторая вкладка).
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/leads/1/language", token,
		gin.H{"language": "es"}), http.StatusOK, "")

	// Язык виден в GET /api/leads (leadJSON).
	m = wantStatus(t, rig.do(t, http.MethodGet, "/api/leads", token, nil), http.StatusOK, "")
	leads := m["leads"].([]any)
	if len(leads) != 1 || leads[0].(map[string]any)["language"] != "es" {
		t.Fatalf("language в GET /api/leads: %v", leads)
	}
}

// TestPatchLanguage_NullInList — язык не определён → в leadJSON он null
// (фронт бейдж не показывает).
func TestPatchLanguage_NullInList(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	m := wantStatus(t, rig.do(t, http.MethodGet, "/api/leads",
		rig.token(t, auth.RoleManager), nil), http.StatusOK, "")
	lead := m["leads"].([]any)[0].(map[string]any)
	if v, has := lead["language"]; !has || v != nil {
		t.Fatalf("до детекции language обязан быть null: %v", lead)
	}
}

func TestPatchLanguage_Validation(t *testing.T) {
	rig := newAPIRig(t, testLead(1, 2))
	token := rig.token(t, auth.RoleManager)

	for name, body := range map[string]any{
		"без language":    gin.H{},
		"вне тройки":      gin.H{"language": "pt"},
		"пустая строка":   gin.H{"language": ""},
		"не строка":       gin.H{"language": 123},
		"верхний регистр": gin.H{"language": "RU"},
	} {
		w := rig.do(t, http.MethodPatch, "/api/leads/1/language", token, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: статус %d, ждали 400; тело: %s", name, w.Code, w.Body.String())
		}
	}
	if evs := rig.languageEvents(); len(evs) != 0 {
		t.Errorf("мусорный PATCH не должен публиковать событий: %+v", evs)
	}

	// Нет лида — 404; стёртый — тоже 404 (как остальные ручки M8).
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/leads/999/language", token,
		gin.H{"language": "ru"}), http.StatusNotFound, "ERR_NOT_FOUND")
	wantStatus(t, rig.do(t, http.MethodDelete, "/api/lgpd/leads/1/erase", token, nil),
		http.StatusOK, "")
	wantStatus(t, rig.do(t, http.MethodPatch, "/api/leads/1/language", token,
		gin.H{"language": "ru"}), http.StatusNotFound, "ERR_NOT_FOUND")
}
