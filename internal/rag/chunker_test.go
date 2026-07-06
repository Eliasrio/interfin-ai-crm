package rag

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitIntoChunks_PacksParagraphs(t *testing.T) {
	text := "Первый абзац.\n\nВторой абзац.\n\nТретий абзац."
	chunks := SplitIntoChunks(text, 1000)
	if len(chunks) != 1 {
		t.Fatalf("короткие абзацы должны упаковаться в один чанк, получили %d: %q", len(chunks), chunks)
	}
	if !strings.Contains(chunks[0], "Первый") || !strings.Contains(chunks[0], "Третий") {
		t.Errorf("чанк потерял абзацы: %q", chunks[0])
	}
}

func TestSplitIntoChunks_RespectsLimit(t *testing.T) {
	para := strings.Repeat("слово ", 100) // ~600 байт (кириллица: 2 байта/буква)
	text := para + "\n\n" + para + "\n\n" + para
	chunks := SplitIntoChunks(text, 700)
	if len(chunks) < 2 {
		t.Fatalf("текст должен разрезаться, получили %d чанков", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > 700 {
			t.Errorf("чанк %d длиной %d байт превышает лимит 700", i, len(c))
		}
		if c == "" {
			t.Errorf("чанк %d пуст", i)
		}
	}
}

func TestSplitIntoChunks_OversizedParagraphWordBoundary(t *testing.T) {
	// Один абзац сильно больше лимита — режется по словам, UTF-8 не рвётся.
	text := strings.TrimSpace(strings.Repeat("длинное_русское_слово ", 200))
	chunks := SplitIntoChunks(text, 300)
	for i, c := range chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("чанк %d порвал UTF-8", i)
		}
		if len(c) > 300 {
			t.Errorf("чанк %d длиной %d байт", i, len(c))
		}
	}
	// Ничего не потеряли: суммарный текст без пробелов совпадает.
	joined := strings.ReplaceAll(strings.Join(chunks, " "), " ", "")
	original := strings.ReplaceAll(text, " ", "")
	if joined != original {
		t.Error("после чанкинга текст потерял содержимое")
	}
}

func TestSplitIntoChunks_EmptyInput(t *testing.T) {
	if got := SplitIntoChunks("", 1000); len(got) != 0 {
		t.Fatalf("пустой текст: %q", got)
	}
	if got := SplitIntoChunks("\n\n \n\n", 1000); len(got) != 0 {
		t.Fatalf("одни разделители: %q", got)
	}
}
