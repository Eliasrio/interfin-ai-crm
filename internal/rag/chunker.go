// Package rag — Retrieval-Augmented Generation (M4, SRS §7.1):
// чанкинг и индексация базы знаний, retrieval с аудитом и fallback.
package rag

import "strings"

// DefaultMaxChunkChars — размер чанка по умолчанию: ~1200 символов ≈ 300
// токенов (оценка len/4 §7.2). top_k=5 таких чанков плюс базовый промпт
// укладываются в бюджет system_prompt = 2000 токенов; перерост дорежет
// Budgeter (truncateToTokens).
const DefaultMaxChunkChars = 1200

// SplitIntoChunks режет текст документа на чанки не длиннее maxChars БАЙТ
// (лимит согласован с оценкой токенов len/4, она тоже байтовая §7.2):
// абзацы пакуются в чанк, пока влезают; сверхдлинный абзац режется жёстко
// по границе слова/руны. Пустых чанков не возвращает.
func SplitIntoChunks(text string, maxChars int) []string {
	if maxChars <= 0 {
		maxChars = DefaultMaxChunkChars
	}

	var chunks []string
	var b strings.Builder
	flush := func() {
		if s := strings.TrimSpace(b.String()); s != "" {
			chunks = append(chunks, s)
		}
		b.Reset()
	}

	text = strings.ReplaceAll(text, "\r\n", "\n")
	for _, para := range strings.Split(text, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		for _, piece := range splitOversized(para, maxChars) {
			// +2 — разделитель "\n\n" между абзацами внутри чанка.
			if b.Len() > 0 && b.Len()+2+len(piece) > maxChars {
				flush()
			}
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(piece)
		}
	}
	flush()
	return chunks
}

// splitOversized режет абзац длиннее maxChars на куски ≤ maxChars,
// предпочитая границу слова; без пробелов в окне — по границе руны
// (тексты русскоязычные, рвать UTF-8 нельзя).
func splitOversized(para string, maxChars int) []string {
	if len(para) <= maxChars {
		return []string{para}
	}
	var parts []string
	rest := para
	for len(rest) > maxChars {
		cut := lastSpaceWithin(rest, maxChars)
		if cut <= 0 {
			cut = runeBoundaryWithin(rest, maxChars)
		}
		parts = append(parts, strings.TrimSpace(rest[:cut]))
		rest = strings.TrimSpace(rest[cut:])
	}
	if rest != "" {
		parts = append(parts, rest)
	}
	return parts
}

// lastSpaceWithin — позиция последнего пробельного байта в первых maxBytes
// байтах строки; -1, если его нет.
func lastSpaceWithin(s string, maxBytes int) int {
	window := s[:maxBytes]
	for i := len(window) - 1; i >= 0; i-- {
		if window[i] == ' ' || window[i] == '\n' || window[i] == '\t' {
			return i
		}
	}
	return -1
}

// runeBoundaryWithin — наибольшая граница руны ≤ maxBytes (минимум одна руна).
func runeBoundaryWithin(s string, maxBytes int) int {
	cut := 0
	for i := range s {
		if i > maxBytes {
			break
		}
		cut = i
	}
	if cut == 0 {
		// Первая руна длиннее окна — отдаём её целиком, чтобы не зациклиться.
		for i := range s {
			if i > 0 {
				return i
			}
		}
		return len(s)
	}
	return cut
}
