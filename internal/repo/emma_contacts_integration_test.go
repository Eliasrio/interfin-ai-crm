// Интеграционные тесты EmmaContactsRepo (EP-05) против реального PostgreSQL
// (схема 0019, POSTGRES_TEST_DSN, иначе skip):
//   - CRUD + порядок (sort_order, новые в конец) + Comment "" → NULL;
//   - критерий приёмки: 31-й активный → ErrContactsLimit и на Create, и на
//     включающем PATCH, ВКЛЮЧАЯ гонку двух конкурентных PATCH (advisory-лок).
package repo

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

func seedContact(t *testing.T, r EmmaContactsRepo, name string, active bool, sortOrder int) *models.EmmaContact {
	t.Helper()
	c := &models.EmmaContact{
		Type: "phone", Name: name, Value: "+7 999 000-00-00",
		IsActive: active, SortOrder: sortOrder,
	}
	if err := r.Create(context.Background(), c); err != nil {
		t.Fatalf("seed contact %s: %v", name, err)
	}
	return c
}

func TestEmmaContactsCRUD_Integration(t *testing.T) {
	gdb := testEmmaDB(t)
	r := NewEmmaContacts(gdb)
	ctx := context.Background()

	first := seedContact(t, r, "Анна", true, 10)
	second := seedContact(t, r, "Офис", true, 5)
	third := seedContact(t, r, "Сайт", false, 10) // тот же sort_order, id больше → в конец

	// List: sort_order ASC, при равенстве новые в конец.
	list, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].ID != second.ID || list[1].ID != first.ID || list[2].ID != third.ID {
		t.Fatalf("порядок List: %+v", list)
	}

	// ListActive — только включённые.
	active, err := r.ListActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 {
		t.Fatalf("active: %+v", active)
	}

	// Update: все поля; Comment "" → NULL.
	comment := "давай при готовности"
	typ, name, value := "whatsapp", "Анна В.", "+55 21 9"
	no := false
	upd, err := r.Update(ctx, first.ID, EmmaContactUpdate{
		Type: &typ, Name: &name, Value: &value, Comment: &comment, IsActive: &no,
	})
	if err != nil {
		t.Fatal(err)
	}
	if upd.Type != "whatsapp" || upd.Name != "Анна В." || upd.Comment == nil || upd.IsActive {
		t.Fatalf("update: %+v", upd)
	}
	empty := ""
	upd, err = r.Update(ctx, first.ID, EmmaContactUpdate{Comment: &empty})
	if err != nil || upd.Comment != nil {
		t.Fatalf("comment должен сброситься в NULL: %+v, %v", upd, err)
	}

	// GetByID / Delete / ErrNotFound.
	if _, err := r.GetByID(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("повторный delete: %v", err)
	}
	if _, err := r.GetByID(ctx, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get стёртого: %v", err)
	}
	if _, err := r.Update(ctx, first.ID, EmmaContactUpdate{Name: &name}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update стёртого: %v", err)
	}
}

// Критерий приёмки: 31-й активный → CONTACTS_LIMIT, включая гонку двух PATCH.
func TestEmmaContactsLimit_Integration(t *testing.T) {
	gdb := testEmmaDB(t)
	r := NewEmmaContacts(gdb)
	ctx := context.Background()

	// 29 активных + 2 выключенных кандидата.
	for i := 0; i < MaxActiveContacts-1; i++ {
		seedContact(t, r, "Контакт", true, i)
	}
	candA := seedContact(t, r, "Кандидат А", false, 100)
	candB := seedContact(t, r, "Кандидат Б", false, 101)

	// Гонка: два конкурентных PATCH is_active=true на 30-е место —
	// advisory-лок пускает ровно одного, второй получает ErrContactsLimit.
	yes := true
	results := make([]error, 2)
	var wg sync.WaitGroup
	for i, id := range []int64{candA.ID, candB.ID} {
		wg.Add(1)
		go func(i int, id int64) {
			defer wg.Done()
			_, results[i] = r.Update(ctx, id, EmmaContactUpdate{IsActive: &yes})
		}(i, id)
	}
	wg.Wait()

	var okCount, limitCount int
	for _, err := range results {
		switch {
		case err == nil:
			okCount++
		case errors.Is(err, ErrContactsLimit):
			limitCount++
		default:
			t.Fatalf("неожиданная ошибка гонки: %v", results)
		}
	}
	if okCount != 1 || limitCount != 1 {
		t.Fatalf("гонка PATCH: ok=%d limit=%d (ждали 1/1)", okCount, limitCount)
	}
	active, err := r.ListActive(ctx)
	if err != nil || len(active) != MaxActiveContacts {
		t.Fatalf("после гонки активных %d, ждали %d (%v)", len(active), MaxActiveContacts, err)
	}

	// Create при полном лимите → ErrContactsLimit; неактивный проходит.
	err = r.Create(ctx, &models.EmmaContact{Type: "other", Name: "31-й", Value: "v", IsActive: true})
	if !errors.Is(err, ErrContactsLimit) {
		t.Fatalf("create сверх лимита: %v", err)
	}
	if err := r.Create(ctx, &models.EmmaContact{Type: "other", Name: "запас", Value: "v"}); err != nil {
		t.Fatalf("неактивный создаваться обязан: %v", err)
	}

	// PATCH активного без is_active лимитом не блокируется; повторное
	// включение уже активного (is_active=true при 30, где он сам в числе
	// активных) — тоже проходит (считаются ДРУГИЕ активные).
	winner := candA.ID
	if results[0] != nil {
		winner = candB.ID
	}
	newName := "Победитель"
	if _, err := r.Update(ctx, winner, EmmaContactUpdate{Name: &newName, IsActive: &yes}); err != nil {
		t.Fatalf("идемпотентное включение активного: %v", err)
	}
}
