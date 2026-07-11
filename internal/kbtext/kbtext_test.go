// Тесты извлечения текста (EP-03 §3): TXT/MD/UTF-8, PDF с текстовым слоем,
// PDF-скан (фикстура без текстовых операторов), битый PDF. Фикстуры —
// testdata/gen.go.
package kbtext

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("фикстура %s: %v (go run ./internal/kbtext/testdata/gen.go)", name, err)
	}
	return data
}

func TestExtractPlain(t *testing.T) {
	for _, mime := range []string{MimeTXT, MimeMD} {
		got, err := Extract(mime, []byte("Секретный факт: код 42.\n"))
		if err != nil {
			t.Fatalf("%s: %v", mime, err)
		}
		if !strings.Contains(got, "код 42") {
			t.Errorf("%s: текст потерян: %q", mime, got)
		}
	}
}

func TestExtractPlainStripsBOM(t *testing.T) {
	got, err := Extract(MimeTXT, []byte("\xef\xbb\xbfтекст"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "текст" {
		t.Errorf("BOM не отрезан: %q", got)
	}
}

func TestExtractPlainNotUTF8(t *testing.T) {
	// cp1251-байты «привет» — валидный win-1251, невалидный UTF-8.
	_, err := Extract(MimeTXT, []byte{0xEF, 0xF0, 0xE8, 0xE2, 0xE5, 0xF2})
	if !errors.Is(err, ErrNotUTF8) {
		t.Fatalf("ждали ErrNotUTF8, получили %v", err)
	}
}

func TestExtractPDFWithTextLayer(t *testing.T) {
	got, err := Extract(MimePDF, fixture(t, "text.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "998877") {
		t.Errorf("текстовый слой не извлечён: %q", got)
	}
}

// PDF-скан → ErrNoTextLayer с текстом ошибки из ТЗ §3 (он показывается
// владельцу в статусе файла и обязан быть понятным).
func TestExtractPDFScan(t *testing.T) {
	_, err := Extract(MimePDF, fixture(t, "scan.pdf"))
	if !errors.Is(err, ErrNoTextLayer) {
		t.Fatalf("ждали ErrNoTextLayer, получили %v", err)
	}
	if !strings.Contains(err.Error(), "OCR не поддерживается") {
		t.Errorf("текст ошибки не из ТЗ §3: %q", err.Error())
	}
}

// Битый PDF — ошибка, не паника (rsc.io/pdf-семейство паникует на мусоре;
// recover в extractPDF обязан её поймать).
func TestExtractPDFGarbage(t *testing.T) {
	for _, data := range [][]byte{
		[]byte("%PDF-1.4\nмусор без структуры"),
		[]byte("вообще не pdf"),
		{},
	} {
		if _, err := Extract(MimePDF, data); err == nil {
			t.Errorf("мусор %q прошёл без ошибки", string(data))
		}
	}
}

func TestExtractUnsupportedMime(t *testing.T) {
	_, err := Extract("application/msword", []byte("x"))
	if !errors.Is(err, ErrUnsupportedMime) {
		t.Fatalf("ждали ErrUnsupportedMime, получили %v", err)
	}
}
