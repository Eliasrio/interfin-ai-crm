// Package kbtext — извлечение текста из файлов базы знаний панели Эммы
// (EP-03, ТЗ §3): TXT/MD читаются как есть (обязателен валидный UTF-8),
// у PDF берётся текстовый слой (pure-Go github.com/ledongthuc/pdf —
// форк rsc.io/pdf с извлечением текста, пришпилен к go 1.22-совместимой
// версии). PDF-скан без текстового слоя — ErrNoTextLayer, OCR вне v1.
//
// Extract — чистая функция без I/O: любая её ошибка перманентна (битый
// файл ретраем не починится), воркер переводит файл в status error
// без ретрая (asynq.SkipRetry).
package kbtext

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// MIME-типы файлов базы знаний (allowlist ТЗ §2.3; пишутся в emma_kb_files).
const (
	MimeTXT = "text/plain"
	MimeMD  = "text/markdown"
	MimePDF = "application/pdf"
)

var (
	// ErrNotUTF8 — TXT/MD не в UTF-8 (или вовсе не текст).
	ErrNotUTF8 = errors.New("файл не является текстом в UTF-8")
	// ErrNoTextLayer — PDF разобран, но текста в нём нет (скан). Текст
	// ошибки — контракт ТЗ §3, показывается владельцу в статусе файла.
	ErrNoTextLayer = errors.New("PDF без текстового слоя, OCR не поддерживается")
	// ErrUnsupportedMime — mime вне allowlist (в норме недостижимо:
	// upload-ручка пропускает только TXT/MD/PDF).
	ErrUnsupportedMime = errors.New("неподдерживаемый тип файла")
)

// Extract извлекает текст документа по mime (константы Mime* выше).
func Extract(mime string, data []byte) (string, error) {
	switch mime {
	case MimeTXT, MimeMD:
		return extractPlain(data)
	case MimePDF:
		return extractPDF(data)
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupportedMime, mime)
	}
}

// extractPlain — TXT/MD как есть; BOM отрезается (частый артефакт Windows).
func extractPlain(data []byte) (string, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return "", ErrNotUTF8
	}
	return string(data), nil
}

// extractPDF — текстовый слой постранично. recover обязателен: rsc.io/pdf
// (и форк) на битых структурах паникует, а не возвращает ошибку — паника
// воркера уронила бы обработчик очереди.
func extractPDF(data []byte) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("PDF не разобран: %v", r)
		}
	}()

	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("PDF не разобран: %w", err)
	}

	var sb strings.Builder
	for i := 1; i <= reader.NumPage(); i++ {
		page := reader.Page(i)
		if page.V.IsNull() {
			continue
		}
		content, perr := page.GetPlainText(nil)
		if perr != nil {
			// Страница без извлекаемого текста (например, только картинка) —
			// пропускаем; итог решает суммарный текст документа ниже.
			continue
		}
		sb.WriteString(content)
		sb.WriteString("\n")
	}

	if strings.TrimSpace(sb.String()) == "" {
		return "", ErrNoTextLayer
	}
	return sb.String(), nil
}
