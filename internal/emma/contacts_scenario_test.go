// Тесты EP-05: ручки контактов (CRUD, валидация, CONTACTS_LIMIT) и
// сценария (GET/PATCH поверх settings, инвариант «кнопка вкл → текст
// непуст»). Гейты 403/401 покрывает общий прогон emmaPaths (emma_test.go);
// лимит 30 в гонке двух PATCH — интеграционный тест репозитория.
package emma

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// memContactsRepo — in-memory repo.EmmaContactsRepo с той же семантикой
// лимита, что у боевого (без advisory-лока — гонку проверяет интеграционный
// тест репозитория).
type memContactsRepo struct {
	mu     sync.Mutex
	nextID int64
	items  map[int64]*models.EmmaContact
}

func newMemContactsRepo() *memContactsRepo {
	return &memContactsRepo{nextID: 1, items: map[int64]*models.EmmaContact{}}
}

func (m *memContactsRepo) activeCountLocked(exclude int64) int {
	n := 0
	for id, c := range m.items {
		if id != exclude && c.IsActive {
			n++
		}
	}
	return n
}

func (m *memContactsRepo) sortedLocked(onlyActive bool) []models.EmmaContact {
	out := make([]models.EmmaContact, 0, len(m.items))
	for _, c := range m.items {
		if onlyActive && !c.IsActive {
			continue
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SortOrder != out[j].SortOrder {
			return out[i].SortOrder < out[j].SortOrder
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (m *memContactsRepo) List(context.Context) ([]models.EmmaContact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sortedLocked(false), nil
}

func (m *memContactsRepo) GetByID(_ context.Context, id int64) (*models.EmmaContact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.items[id]
	if !ok {
		return nil, repo.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (m *memContactsRepo) Create(_ context.Context, c *models.EmmaContact) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.IsActive && m.activeCountLocked(0) >= repo.MaxActiveContacts {
		return repo.ErrContactsLimit
	}
	c.ID = m.nextID
	m.nextID++
	cp := *c
	m.items[c.ID] = &cp
	return nil
}

func (m *memContactsRepo) Update(_ context.Context, id int64, upd repo.EmmaContactUpdate) (*models.EmmaContact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.items[id]
	if !ok {
		return nil, repo.ErrNotFound
	}
	if upd.IsActive != nil && *upd.IsActive && m.activeCountLocked(id) >= repo.MaxActiveContacts {
		return nil, repo.ErrContactsLimit
	}
	if upd.Type != nil {
		c.Type = *upd.Type
	}
	if upd.Name != nil {
		c.Name = *upd.Name
	}
	if upd.Value != nil {
		c.Value = *upd.Value
	}
	if upd.Comment != nil {
		if *upd.Comment == "" {
			c.Comment = nil
		} else {
			v := *upd.Comment
			c.Comment = &v
		}
	}
	if upd.IsActive != nil {
		c.IsActive = *upd.IsActive
	}
	if upd.SortOrder != nil {
		c.SortOrder = *upd.SortOrder
	}
	cp := *c
	return &cp, nil
}

func (m *memContactsRepo) Delete(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[id]; !ok {
		return repo.ErrNotFound
	}
	delete(m.items, id)
	return nil
}

func (m *memContactsRepo) ListActive(context.Context) ([]models.EmmaContact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sortedLocked(true), nil
}

// --- контакты ---

// adminRig — риг с открытой PIN-сессией.
func adminRig(t *testing.T) (*rig, string) {
	t.Helper()
	rg := newRig(t, newFakeStore())
	admin := rg.token(t, auth.RoleAdmin)
	setupPIN(t, rg, admin, "123456")
	return rg, admin
}

func TestContactsCRUD(t *testing.T) {
	rg, admin := adminRig(t)

	// POST: валидный контакт → 201 со всеми полями.
	w := rg.do(t, http.MethodPost, "/api/emma/contacts", admin, gin.H{
		"type": "phone", "name": "Менеджер Анна", "value": "+7 999 123-45-67",
		"comment": "давай, когда клиент готов", "sort_order": 5,
	})
	created := wantStatus(t, w, http.StatusCreated, "")
	if created["type"] != "phone" || created["name"] != "Менеджер Анна" ||
		created["is_active"] != true || created["sort_order"] != float64(5) {
		t.Fatalf("создание: %v", created)
	}
	id := int64(created["id"].(float64))

	// Валидация: type вне enum, пустые name/value.
	for name, body := range map[string]gin.H{
		"неизвестный type": {"type": "fax", "name": "n", "value": "v"},
		"пустой name":      {"type": "phone", "name": "  ", "value": "v"},
		"пустой value":     {"type": "phone", "name": "n", "value": ""},
	} {
		w := rg.do(t, http.MethodPost, "/api/emma/contacts", admin, body)
		wantStatus(t, w, http.StatusBadRequest, "ERR_VALIDATION")
		_ = name
	}

	// PATCH: правка значения и выключение.
	w = rg.do(t, http.MethodPatch, "/api/emma/contacts/1", admin, gin.H{
		"value": "+55 21 0000-00-00", "is_active": false, "comment": "",
	})
	patched := wantStatus(t, w, http.StatusOK, "")
	if patched["value"] != "+55 21 0000-00-00" || patched["is_active"] != false ||
		patched["comment"] != "" {
		t.Fatalf("PATCH: %v", patched)
	}

	// GET: список с limit для счётчика вкладки.
	list := wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/contacts", admin, nil),
		http.StatusOK, "")
	if list["limit"] != float64(repo.MaxActiveContacts) {
		t.Errorf("limit: %v", list["limit"])
	}
	if items := list["items"].([]any); len(items) != 1 {
		t.Errorf("items: %v", items)
	}

	// DELETE → 204, повторный → 404.
	if w := rg.do(t, http.MethodDelete, "/api/emma/contacts/1", admin, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	wantStatus(t, rg.do(t, http.MethodDelete, "/api/emma/contacts/1", admin, nil),
		http.StatusNotFound, "ERR_NOT_FOUND")
	_ = id
}

// Критерий приёмки: 31-й активный → 400 CONTACTS_LIMIT (и POST, и PATCH,
// включающий is_active); limit в теле ответа.
func TestContactsLimit(t *testing.T) {
	rg, admin := adminRig(t)
	for i := 0; i < repo.MaxActiveContacts; i++ {
		w := rg.do(t, http.MethodPost, "/api/emma/contacts", admin, gin.H{
			"type": "other", "name": "Контакт", "value": "v",
		})
		wantStatus(t, w, http.StatusCreated, "")
	}

	// 31-й активный POST'ом → 400 CONTACTS_LIMIT.
	w := rg.do(t, http.MethodPost, "/api/emma/contacts", admin, gin.H{
		"type": "other", "name": "31-й", "value": "v",
	})
	body := wantStatus(t, w, http.StatusBadRequest, CodeContactsLimit)
	if body["limit"] != float64(repo.MaxActiveContacts) {
		t.Errorf("limit в теле: %v", body)
	}

	// Неактивный создаётся, включение его PATCH'ем → 400 CONTACTS_LIMIT.
	w = rg.do(t, http.MethodPost, "/api/emma/contacts", admin, gin.H{
		"type": "other", "name": "запасной", "value": "v", "is_active": false,
	})
	created := wantStatus(t, w, http.StatusCreated, "")
	spareID := int(created["id"].(float64))
	w = rg.do(t, http.MethodPatch, "/api/emma/contacts/"+strconv.Itoa(spareID), admin,
		gin.H{"is_active": true})
	wantStatus(t, w, http.StatusBadRequest, CodeContactsLimit)

	// PATCH уже-активного, НЕ трогающий is_active, лимитом не блокируется.
	w = rg.do(t, http.MethodPatch, "/api/emma/contacts/1", admin, gin.H{"name": "новое имя"})
	wantStatus(t, w, http.StatusOK, "")

	// Выключили один — включение запасного проходит.
	w = rg.do(t, http.MethodPatch, "/api/emma/contacts/1", admin, gin.H{"is_active": false})
	wantStatus(t, w, http.StatusOK, "")
	w = rg.do(t, http.MethodPatch, "/api/emma/contacts/"+strconv.Itoa(spareID), admin,
		gin.H{"is_active": true})
	wantStatus(t, w, http.StatusOK, "")
}

// --- сценарий ---

func TestScenarioGetDefaults(t *testing.T) {
	rg, admin := adminRig(t)
	got := wantStatus(t, rg.do(t, http.MethodGet, "/api/emma/scenario", admin, nil),
		http.StatusOK, "")
	if got["welcome_text"] != "" || got["manager_button_enabled"] != false ||
		got["manager_button_text"] != "Связаться с менеджером" ||
		got["handoff_confirm_text"] != "Сейчас свяжу вас с менеджером, ожидайте" {
		t.Fatalf("дефолты вкладки 5: %v", got)
	}
}

func TestScenarioPatch(t *testing.T) {
	rg, admin := adminRig(t)

	// Частичное обновление: только welcome, остальные ключи не тронуты.
	got := wantStatus(t, rg.do(t, http.MethodPatch, "/api/emma/scenario", admin,
		gin.H{"welcome_text": "  Привет! Я Эмма.  "}), http.StatusOK, "")
	if got["welcome_text"] != "Привет! Я Эмма." {
		t.Fatalf("welcome не сохранён (и не оттриммлен): %v", got)
	}
	if got["manager_button_enabled"] != false {
		t.Errorf("PATCH welcome тронул кнопку: %v", got)
	}

	// Включение кнопки с дефолтным (непустым) текстом — ок.
	got = wantStatus(t, rg.do(t, http.MethodPatch, "/api/emma/scenario", admin,
		gin.H{"manager_button_enabled": true}), http.StatusOK, "")
	if got["manager_button_enabled"] != true {
		t.Fatalf("кнопка не включилась: %v", got)
	}

	// Инвариант: стирание текста при включённой кнопке → 400; включение
	// вместе с пустым текстом → 400.
	wantStatus(t, rg.do(t, http.MethodPatch, "/api/emma/scenario", admin,
		gin.H{"manager_button_text": "   "}), http.StatusBadRequest, "ERR_VALIDATION")
	wantStatus(t, rg.do(t, http.MethodPatch, "/api/emma/scenario", admin,
		gin.H{"manager_button_enabled": true, "manager_button_text": ""}),
		http.StatusBadRequest, "ERR_VALIDATION")

	// Выключить кнопку и стереть текст одним PATCH — легально.
	got = wantStatus(t, rg.do(t, http.MethodPatch, "/api/emma/scenario", admin,
		gin.H{"manager_button_enabled": false, "manager_button_text": ""}),
		http.StatusOK, "")
	if got["manager_button_enabled"] != false || got["manager_button_text"] != "" {
		t.Fatalf("выключение со стиранием: %v", got)
	}

	// Пустое тело → 400.
	wantStatus(t, rg.do(t, http.MethodPatch, "/api/emma/scenario", admin, gin.H{}),
		http.StatusBadRequest, "ERR_VALIDATION")
}
