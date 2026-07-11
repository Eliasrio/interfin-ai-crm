// Интеграционные тесты EP-02 против реального PostgreSQL (схема 0016,
// POSTGRES_TEST_DSN, иначе skip):
//   - CreateVersion переключает активную версию транзакционно;
//   - критерий приёмки: параллельные PUT не дают двух активных (частичный
//     уникальный индекс emma_prompt_current_key);
//   - превью истории режется по рунам (LEFT), кириллица не рвётся.
package repo

import (
	"context"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

func testPromptRepo(t *testing.T) EmmaPromptRepo {
	t.Helper()
	gdb := testDB(t)
	if err := gdb.Exec(
		`TRUNCATE emma_prompt_versions RESTART IDENTITY CASCADE`,
	).Error; err != nil {
		t.Fatalf("truncate emma_prompt_versions: %v (миграция 0016 накатана?)", err)
	}
	return NewEmmaPrompts(gdb)
}

func promptVersion(text, topics, style string) *models.EmmaPromptVersion {
	return &models.EmmaPromptVersion{
		SystemPrompt:    text,
		ForbiddenTopics: models.JSONB(topics),
		Style:           style,
	}
}

// TestEmmaPromptVersionSwitch — CreateVersion делает новую версию активной,
// прежняя остаётся в истории; GetByID отдаёт обе; restore-семантика (копия
// полей) переключает активную ещё раз.
func TestEmmaPromptVersionSwitch(t *testing.T) {
	prompts := testPromptRepo(t)
	ctx := context.Background()

	if _, err := prompts.GetCurrent(ctx); err == nil {
		t.Fatal("GetCurrent на пустой таблице обязан отдавать ErrNotFound")
	}

	v1 := promptVersion("Версия один.", `["политика"]`, models.EmmaStyleExpert)
	if err := prompts.CreateVersion(ctx, v1); err != nil {
		t.Fatal(err)
	}
	if v1.ID == 0 || v1.CreatedAt.IsZero() {
		t.Fatalf("CreateVersion не заполнил ID/CreatedAt: %+v", v1)
	}
	v2 := promptVersion("Версия два.", `[]`, models.EmmaStyleNeutral)
	if err := prompts.CreateVersion(ctx, v2); err != nil {
		t.Fatal(err)
	}

	cur, err := prompts.GetCurrent(ctx)
	if err != nil || cur.ID != v2.ID {
		t.Fatalf("активная: %+v, %v — ждали v2", cur, err)
	}
	old, err := prompts.GetByID(ctx, v1.ID)
	if err != nil || old.IsCurrent {
		t.Fatalf("v1 обязана остаться в истории неактивной: %+v, %v", old, err)
	}

	// Restore = CreateVersion с копией полей источника (строки не мутируются).
	restored := promptVersion(old.SystemPrompt, string(old.ForbiddenTopics), old.Style)
	if err := prompts.CreateVersion(ctx, restored); err != nil {
		t.Fatal(err)
	}
	cur, err = prompts.GetCurrent(ctx)
	if err != nil || cur.ID != restored.ID || cur.SystemPrompt != "Версия один." ||
		cur.Style != models.EmmaStyleExpert || string(cur.ForbiddenTopics) != `["политика"]` {
		t.Fatalf("restore: активная %+v, %v", cur, err)
	}
}

// TestEmmaPromptConcurrentCreate — критерий приёмки EP-02: параллельные PUT
// не дают двух активных. Проигравшие гонку транзакции могут получить ошибку
// уникального индекса — это контракт (не молчаливая перезапись); активная
// после любой развязки ровно одна.
func TestEmmaPromptConcurrentCreate(t *testing.T) {
	gdb := testDB(t)
	if err := gdb.Exec(`TRUNCATE emma_prompt_versions RESTART IDENTITY CASCADE`).Error; err != nil {
		t.Fatal(err)
	}
	prompts := NewEmmaPrompts(gdb)
	ctx := context.Background()

	const writers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := prompts.CreateVersion(ctx,
				promptVersion("Конкурентная версия.", `[]`, models.EmmaStyleNeutral))
			if err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if succeeded == 0 {
		t.Fatal("ни один конкурентный PUT не прошёл")
	}
	var actives int64
	if err := gdb.Model(&models.EmmaPromptVersion{}).
		Where("is_current").Count(&actives).Error; err != nil {
		t.Fatal(err)
	}
	if actives != 1 {
		t.Fatalf("активных версий %d, ждали ровно 1 (индекс 0016)", actives)
	}
	var rows int64
	if err := gdb.Model(&models.EmmaPromptVersion{}).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != int64(succeeded) {
		t.Errorf("строк %d при %d успешных вставках — проигравшие обязаны откатываться целиком",
			rows, succeeded)
	}
}

// TestEmmaPromptHistoryPreviewRunes — превью истории: первые 100 СИМВОЛОВ
// кириллицы (не байтов), сортировка новые → старые, total и страницы.
func TestEmmaPromptHistoryPreviewRunes(t *testing.T) {
	prompts := testPromptRepo(t)
	ctx := context.Background()

	long := strings.Repeat("ю", 150)
	for _, text := range []string{"Первая.", long} {
		if err := prompts.CreateVersion(ctx,
			promptVersion(text, `[]`, models.EmmaStyleNeutral)); err != nil {
			t.Fatal(err)
		}
	}

	items, total, err := prompts.History(ctx, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("total=%d items=%d", total, len(items))
	}
	if items[0].ID <= items[1].ID {
		t.Fatalf("история не отсортирована новые → старые: %d, %d", items[0].ID, items[1].ID)
	}
	preview := items[0].Preview
	if utf8.RuneCountInString(preview) != 100 || !utf8.ValidString(preview) ||
		preview != strings.Repeat("ю", 100) {
		t.Fatalf("превью не 100 рун / рвёт кириллицу: %q (байт: %d)", preview, len(preview))
	}

	// Пагинация: страница за пределами — пустая, total прежний.
	items, total, err = prompts.History(ctx, 2, 50)
	if err != nil || total != 2 || len(items) != 0 {
		t.Fatalf("вторая страница: items=%d total=%d err=%v", len(items), total, err)
	}
}
