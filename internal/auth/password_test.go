package auth

import (
	"strings"
	"testing"
)

// Критерий приёмки M7: пароли не хранятся в открытом виде.
func TestHashPasswordNotPlaintext(t *testing.T) {
	const pw = "s3cret-пароль"
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, pw) {
		t.Fatal("хеш содержит пароль в открытом виде")
	}
	if !strings.HasPrefix(hash, "$2") {
		t.Fatalf("не bcrypt-хеш: %q", hash)
	}
	if !CheckPassword(hash, pw) {
		t.Fatal("верный пароль не прошёл сверку")
	}
	if CheckPassword(hash, "другой") {
		t.Fatal("неверный пароль прошёл сверку")
	}
}

// bcrypt солёный: один пароль — разные хеши (иначе утечка таблицы
// вскрывала бы совпадающие пароли пачкой).
func TestHashPasswordSalted(t *testing.T) {
	h1, err := HashPassword("одинаковый")
	if err != nil {
		t.Fatal(err)
	}
	h2, err := HashPassword("одинаковый")
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatal("хеши совпали — соль не работает")
	}
}

func TestHashPasswordTooLong(t *testing.T) {
	if _, err := HashPassword(strings.Repeat("a", 73)); err == nil {
		t.Fatal("пароль длиннее 72 байт обязан отклоняться (bcrypt его обрезал бы)")
	}
}
