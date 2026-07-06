package repo

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/models"
)

// Интеграционные тесты M7 против реального PostgreSQL (схема 0009).
// Как и остальные: без POSTGRES_TEST_DSN — skip.

func testAuthRepos(t *testing.T) (ManagerRepo, RefreshTokenRepo) {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN не задан — интеграционный тест пропущен")
	}
	gdb, err := db.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec("TRUNCATE refresh_tokens, managers RESTART IDENTITY CASCADE").Error; err != nil {
		t.Fatal(err)
	}
	managers, tokens := NewAuth(gdb)
	return managers, tokens
}

func testManager(t *testing.T, managers ManagerRepo, email, password string, active bool) *models.Manager {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	m := &models.Manager{
		Email:        email,
		PasswordHash: hash,
		Role:         auth.RoleManager,
		Active:       active,
	}
	if err := managers.Create(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestManagerRepoIntegration(t *testing.T) {
	managers, _ := testAuthRepos(t)
	ctx := context.Background()

	created := testManager(t, managers, "m7@interfin.com", "p@ssw0rd-длинный", true)
	if created.ID == 0 {
		t.Fatal("Create не заполнил ID")
	}

	got, err := managers.GetByEmail(ctx, "m7@interfin.com")
	if err != nil {
		t.Fatal(err)
	}
	// Критерий приёмки M7: в БД нет пароля в открытом виде.
	if got.PasswordHash == "p@ssw0rd-длинный" {
		t.Fatal("в БД лежит пароль в открытом виде")
	}
	if !auth.CheckPassword(got.PasswordHash, "p@ssw0rd-длинный") {
		t.Fatal("bcrypt-сверка выданного хеша не прошла")
	}

	// Деактивированный менеджер невидим для выборок логина/refresh.
	inactive := testManager(t, managers, "off@interfin.com", "p@ssw0rd-другой", false)
	if _, err := managers.GetByEmail(ctx, "off@interfin.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByEmail неактивного: %v, ожидался ErrNotFound", err)
	}
	if _, err := managers.GetByID(ctx, inactive.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID неактивного: %v, ожидался ErrNotFound", err)
	}

	// UNIQUE email — дубль отклоняется БД.
	dup := &models.Manager{Email: "m7@interfin.com", PasswordHash: "x",
		Role: auth.RoleManager, Active: true}
	if err := managers.Create(ctx, dup); err == nil {
		t.Fatal("дубль email прошёл вопреки UNIQUE")
	}
}

func TestRefreshTokenRepoIntegration(t *testing.T) {
	managers, tokens := testAuthRepos(t)
	ctx := context.Background()
	m := testManager(t, managers, "rt@interfin.com", "p@ssw0rd-третий", true)

	token := auth.NewRefreshToken()
	hash := auth.HashRefreshToken(token)
	err := tokens.Create(ctx, &models.RefreshToken{
		TokenHash: hash,
		ManagerID: m.ID,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Consume возвращает строку и гасит её; повтор — ErrNotFound (ротация).
	rt, err := tokens.Consume(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if rt.ManagerID != m.ID {
		t.Fatalf("manager_id = %d, ожидался %d", rt.ManagerID, m.ID)
	}
	if _, err := tokens.Consume(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("повторный Consume: %v, ожидался ErrNotFound", err)
	}

	// DeleteExpired убирает только просроченные.
	fresh := auth.HashRefreshToken(auth.NewRefreshToken())
	stale := auth.HashRefreshToken(auth.NewRefreshToken())
	for hash, ttl := range map[string]time.Duration{fresh: time.Hour, stale: -time.Hour} {
		err := tokens.Create(ctx, &models.RefreshToken{
			TokenHash: hash, ManagerID: m.ID, ExpiresAt: time.Now().Add(ttl)})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := tokens.DeleteExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.Consume(ctx, stale); !errors.Is(err, ErrNotFound) {
		t.Fatalf("просроченный пережил DeleteExpired: %v", err)
	}
	if _, err := tokens.Consume(ctx, fresh); err != nil {
		t.Fatalf("живой токен удалён DeleteExpired: %v", err)
	}
}
