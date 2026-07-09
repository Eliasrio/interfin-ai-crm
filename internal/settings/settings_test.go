// Юнит-тесты сервиса настроек (M13): дефолты, кэш 30с, валидация.
package settings

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// fakeRepo — in-memory repo.SettingsRepo со счётчиком чтений (проверка кэша).
type fakeRepo struct {
	mu     sync.Mutex
	values map[string]string
	gets   int
	getErr error
}

func newFakeRepo() *fakeRepo { return &fakeRepo{values: map[string]string{}} }

func (f *fakeRepo) Get(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return "", f.getErr
	}
	v, ok := f.values[key]
	if !ok {
		return "", repo.ErrNotFound
	}
	return v, nil
}

func (f *fakeRepo) Set(_ context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[key] = value
	return nil
}

func TestMinutes_DefaultWhenMissing(t *testing.T) {
	svc := New(newFakeRepo())
	ctx := context.Background()
	if got := svc.Minutes(ctx, KeyHybridPauseMinutes); got != 30 {
		t.Errorf("hybrid_pause default = %d, ожидали 30", got)
	}
	if got := svc.Minutes(ctx, KeyReminderMinutes); got != 10 {
		t.Errorf("reminder default = %d, ожидали 10", got)
	}
	if got := svc.Minutes(ctx, KeyPickupMinutes); got != 10 {
		t.Errorf("pickup default = %d, ожидали 10", got)
	}
}

func TestMinutes_OverrideAndCache(t *testing.T) {
	r := newFakeRepo()
	r.values[KeyReminderMinutes] = "5"
	svc := New(r)
	now := time.Now()
	svc.now = func() time.Time { return now }
	ctx := context.Background()

	if got := svc.Minutes(ctx, KeyReminderMinutes); got != 5 {
		t.Fatalf("override = %d, ожидали 5", got)
	}
	// Второе чтение внутри TTL — из кэша, БД не трогается.
	svc.Minutes(ctx, KeyReminderMinutes)
	if r.gets != 1 {
		t.Errorf("чтений БД %d, ожидали 1 (кэш 30с)", r.gets)
	}
	// TTL истёк — чтение уходит в БД снова.
	now = now.Add(31 * time.Second)
	svc.Minutes(ctx, KeyReminderMinutes)
	if r.gets != 2 {
		t.Errorf("чтений БД %d, ожидали 2 (кэш истёк)", r.gets)
	}
}

func TestMinutes_GarbageValueFallsBackToDefault(t *testing.T) {
	r := newFakeRepo()
	r.values[KeyPickupMinutes] = "abc"
	if got := New(r).Minutes(context.Background(), KeyPickupMinutes); got != 10 {
		t.Errorf("мусор в БД: %d, ожидали дефолт 10", got)
	}
}

func TestMinutes_RepoErrorNotCached(t *testing.T) {
	r := newFakeRepo()
	r.getErr = errors.New("db down")
	svc := New(r)
	ctx := context.Background()
	if got := svc.Minutes(ctx, KeyReminderMinutes); got != 10 {
		t.Fatalf("при ошибке БД ожидали дефолт 10, got %d", got)
	}
	// Ошибка не закэширована: следующее чтение снова идёт в БД.
	r.getErr = nil
	r.values[KeyReminderMinutes] = "7"
	if got := svc.Minutes(ctx, KeyReminderMinutes); got != 7 {
		t.Errorf("после восстановления БД ожидали 7, got %d", got)
	}
}

// TestMinutes_ZeroFromDB — «настройка в 0 минут» (прямой сид в БД для e2e):
// сервис читает 0 как есть — пауза NOW()+0 истекает мгновенно.
func TestMinutes_ZeroFromDB(t *testing.T) {
	r := newFakeRepo()
	r.values[KeyHybridPauseMinutes] = "0"
	if got := New(r).Minutes(context.Background(), KeyHybridPauseMinutes); got != 0 {
		t.Errorf("0 из БД: %d, ожидали 0 (не дефолт)", got)
	}
}

func TestSetMinutes_ValidatesAndInvalidatesCache(t *testing.T) {
	r := newFakeRepo()
	svc := New(r)
	ctx := context.Background()

	// Прогреваем кэш дефолтом, меняем — новое значение видно сразу.
	if got := svc.Minutes(ctx, KeyReminderMinutes); got != 10 {
		t.Fatalf("дефолт = %d", got)
	}
	if err := svc.SetMinutes(ctx, KeyReminderMinutes, 3); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := svc.Minutes(ctx, KeyReminderMinutes); got != 3 {
		t.Errorf("после Set = %d, ожидали 3 (кэш обязан сброситься)", got)
	}

	for name, tc := range map[string]struct {
		key     string
		val     int
		wantErr error
	}{
		"ноль":             {KeyReminderMinutes, 0, ErrOutOfRange},
		"отрицательное":    {KeyReminderMinutes, -5, ErrOutOfRange},
		"больше суток":     {KeyReminderMinutes, 1441, ErrOutOfRange},
		"неизвестный ключ": {"takeover.nope", 10, ErrUnknownKey},
	} {
		if err := svc.SetMinutes(ctx, tc.key, tc.val); !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: err = %v, ожидали %v", name, err, tc.wantErr)
		}
	}
}
