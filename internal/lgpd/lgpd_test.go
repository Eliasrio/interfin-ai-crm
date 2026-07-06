// Юнит-тесты HashTelegramUserID — свойства, на которых держится §9.3:
// детерминированность (идемпотентный erasure), зависимость от соли
// (необратимость без секрета) и строгая отрицательность (невозможность
// коллизии с живым Telegram ID).
package lgpd

import "testing"

func TestHashDeterministic(t *testing.T) {
	a := HashTelegramUserID(123456789, "salt")
	b := HashTelegramUserID(123456789, "salt")
	if a != b {
		t.Fatalf("хеш недетерминирован: %d != %d", a, b)
	}
}

func TestHashDependsOnSaltAndID(t *testing.T) {
	base := HashTelegramUserID(123456789, "salt")
	if HashTelegramUserID(123456789, "other-salt") == base {
		t.Fatal("хеш не зависит от соли")
	}
	if HashTelegramUserID(987654321, "salt") == base {
		t.Fatal("хеш не зависит от user_id")
	}
}

func TestHashAlwaysNegative(t *testing.T) {
	// Telegram user_id всегда > 0 — отрицательный хеш не столкнётся с живым
	// пользователем и не «воскресит» стёртого лида в ingestion-пайплайне.
	for _, id := range []int64{1, 42, 123456789, 1<<62 + 12345} {
		if h := HashTelegramUserID(id, "salt"); h >= 0 {
			t.Fatalf("хеш для %d неотрицателен: %d", id, h)
		}
	}
}
