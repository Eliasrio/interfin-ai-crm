// Интеграционный тест сида 0021 (EP-02) против реального PostgreSQL
// (POSTGRES_TEST_DSN, иначе skip). Живёт в worker, а не в repo: сверка
// «GetCurrent = текст константы» требует видеть неэкспортируемую константу
// systemPrompt (repo импортировать worker не может — цикл).
//
// Критерий приёмки: после 0021 GetCurrent отдаёт текст константы, и Эмма
// отвечает как до эпика — system-блок, собранный из БД, побайтово равен
// собранному из fallback-константы.
package worker

import (
	"context"
	"os"
	"testing"

	"gorm.io/gorm"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

const seedPath = "../../migrations/0021_emma_prompt_seed.up.sql"

func seedTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN не задан — интеграционный тест пропущен")
	}
	gdb, err := db.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec(`TRUNCATE emma_prompt_versions RESTART IDENTITY CASCADE`).Error; err != nil {
		t.Fatalf("truncate: %v (миграция 0016 накатана?)", err)
	}
	return gdb
}

func applySeed(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	sql, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatalf("сид 0021 не прочитан: %v", err)
	}
	if err := gdb.Exec(string(sql)).Error; err != nil {
		t.Fatalf("сид 0021 не накатился: %v", err)
	}
}

// TestSeed0021_CurrentEqualsConstant — GetCurrent после сида = текст
// константы (байт в байт), style neutral, темы пусты, created_by NULL;
// system-блок для Claude не отличается от доэпохального.
func TestSeed0021_CurrentEqualsConstant(t *testing.T) {
	gdb := seedTestDB(t)
	applySeed(t, gdb)

	prompts := repo.NewEmmaPrompts(gdb)
	cur, err := prompts.GetCurrent(context.Background())
	if err != nil {
		t.Fatalf("GetCurrent после 0021: %v", err)
	}
	if cur.SystemPrompt != systemPrompt {
		t.Error("текст сида 0021 разошёлся с константой prompt.go — " +
			"поведение Эммы при деплое изменится (правь миграцию)")
	}
	if cur.Style != models.EmmaStyleNeutral || string(cur.ForbiddenTopics) != "[]" ||
		cur.CreatedBy != nil || !cur.IsCurrent {
		t.Fatalf("поля сида: %+v", cur)
	}

	// Смоук поведения: system-блок из БД == system-блок на константе.
	provider := NewPromptProvider(prompts, testLogger())
	cfg, err := provider.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if buildSystemBase(cfg, nil) != buildSystemBase(fallbackPromptConfig(), nil) {
		t.Error("system-блок из БД отличается от доэпохального — Эмма ответит иначе")
	}
}

// TestSeed0021_IdempotentAndGuarded — повторный накат не плодит строк;
// при уже сохранённых владельцем версиях сид — no-op (guard NOT EXISTS).
func TestSeed0021_IdempotentAndGuarded(t *testing.T) {
	gdb := seedTestDB(t)

	applySeed(t, gdb)
	applySeed(t, gdb) // идемпотентность
	var rows int64
	if err := gdb.Model(&models.EmmaPromptVersion{}).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("после двойного сида строк %d, ждали 1", rows)
	}

	// Владелец уже сохранял версии: сид не затирает его данные.
	if err := gdb.Exec(`TRUNCATE emma_prompt_versions RESTART IDENTITY CASCADE`).Error; err != nil {
		t.Fatal(err)
	}
	prompts := repo.NewEmmaPrompts(gdb)
	owners := &models.EmmaPromptVersion{
		SystemPrompt:    "Промпт владельца.",
		ForbiddenTopics: models.JSONB(`[]`),
		Style:           models.EmmaStyleFriendly,
	}
	if err := prompts.CreateVersion(context.Background(), owners); err != nil {
		t.Fatal(err)
	}
	applySeed(t, gdb)
	cur, err := prompts.GetCurrent(context.Background())
	if err != nil || cur.SystemPrompt != "Промпт владельца." {
		t.Fatalf("сид затёр версию владельца: %+v, %v", cur, err)
	}
}
