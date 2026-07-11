//go:build ignore

// gen.go — генератор PDF-фикстур kbtext (EP-03). Запуск из корня репо:
//
//	go run ./internal/kbtext/testdata/gen.go
//
// Пишет в internal/kbtext/testdata:
//   - text.pdf — одна страница с текстовым слоем (Helvetica, латиница —
//     кириллица требовала бы встраивания шрифта, для фикстуры это лишнее);
//   - scan.pdf — та же структура, но в контенте только заливка прямоугольника,
//     ни одного текстового оператора — модель «скана» без текстового слоя.
//
// PDF собирается вручную с честным xref (смещения считаются кодом):
// rsc.io/pdf-семейство строго к структуре файла.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	dir := "internal/kbtext/testdata"
	if _, err := os.Stat(dir); err != nil {
		fmt.Fprintln(os.Stderr, "запускать из корня репозитория:", err)
		os.Exit(1)
	}

	textContent := "BT /F1 12 Tf 72 720 Td (INTERFIN SECRET CODE 998877) Tj ET"
	scanContent := "0 0 612 792 re f"

	write(filepath.Join(dir, "text.pdf"), buildPDF(textContent, true))
	write(filepath.Join(dir, "scan.pdf"), buildPDF(scanContent, false))
}

func write(path string, data []byte) {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("written", path, len(data), "bytes")
}

// buildPDF — минимальный однострочный PDF 1.4: Catalog → Pages → Page →
// Contents (+ Font, если withFont). Смещения xref считаются по факту.
func buildPDF(content string, withFont bool) []byte {
	var buf bytes.Buffer
	offsets := make([]int, 0, 6)

	obj := func(body string) {
		offsets = append(offsets, buf.Len())
		buf.WriteString(body)
	}

	buf.WriteString("%PDF-1.4\n")

	resources := ""
	if withFont {
		resources = " /Resources << /Font << /F1 5 0 R >> >>"
	}

	obj("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	obj("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	obj(fmt.Sprintf(
		"3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R%s >>\nendobj\n",
		resources))
	obj(fmt.Sprintf(
		"4 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n",
		len(content), content))
	if withFont {
		obj("5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n")
	}

	xrefOffset := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(offsets)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf,
		"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(offsets)+1, xrefOffset)

	return buf.Bytes()
}
