package lang

import "testing"

// TestDetect — юнит-таблица task M14 §2 (+ пограничные случаи).
func TestDetect(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"Привет", RU},
		{"Hi", EN},
		{"Hola", ES},
		{"Buenas tardes, quiero info", ES},
		{"hello, I need citizenship info", EN},
		{"Hi, I need info", EN},
		{"Hola, quiero la ciudadanía", ES},
		{"👍🙏", RU},            // эмодзи-только → fallback ru
		{"", RU},              // пустая строка → fallback ru
		{"   ", RU},           // пробелы → fallback ru
		{"12345", RU},         // цифры: язык не определим → fallback ru
		{"Привет, hola!", RU}, // кириллица есть → быстрый путь ru
	}
	for _, c := range cases {
		if got := Detect(c.text); got != c.want {
			t.Errorf("Detect(%q) = %q, ожидали %q", c.text, got, c.want)
		}
	}
}

// TestValid — валидация тройки для PATCH-ручки.
func TestValid(t *testing.T) {
	for _, ok := range []string{"ru", "en", "es"} {
		if !Valid(ok) {
			t.Errorf("Valid(%q) = false, ожидали true", ok)
		}
	}
	for _, bad := range []string{"", "pt", "RU", "rus", "123"} {
		if Valid(bad) {
			t.Errorf("Valid(%q) = true, ожидали false", bad)
		}
	}
}
