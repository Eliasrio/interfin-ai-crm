// Package lang — язык клиента (M14): ровно три языка ru / en / es
// (решение владельца, NEXT_STEPS 2.1; португальский НЕ нужен).
//
// Detect определяет язык ПЕРВОГО текстового сообщения лида (ingestion, M2);
// дальше язык не пересматривается автоматически — только руками менеджера.
// Детекция локальная и быстрая: правило «webhook отвечает 200 немедленно»
// (CLAUDE.md §4.4) не нарушается.
package lang

import (
	"strings"
	"unicode"

	"github.com/abadojack/whatlanggo"
)

// Коды языков — значения leads.language (CHECK в 0015).
const (
	RU = "ru"
	EN = "en"
	ES = "es"
)

// Supported — тройка целиком (M15 сделает список управляемым, здесь —
// constant по task M14 §«Вне скоупа»).
var Supported = []string{RU, EN, ES}

// Valid — код входит в тройку (валидация PATCH /api/leads/:id/language).
func Valid(code string) bool {
	return code == RU || code == EN || code == ES
}

// latinWhitelist — кандидаты для триграмм whatlanggo: кириллицу ловит
// быстрый путь ниже, так что выбор только между en и es.
var latinWhitelist = whatlanggo.Options{Whitelist: map[whatlanggo.Lang]bool{
	whatlanggo.Eng: true,
	whatlanggo.Spa: true,
}}

// Detect — язык текста: 'ru' | 'en' | 'es'.
//
//   - есть кириллица → сразу 'ru': короткому «Привет» триграммы не нужны,
//     а русскоязычная аудитория — основная;
//   - иначе whatlanggo с whitelist {en, es}. Порог по Info.Confidence
//     НЕ используется: для коротких фраз («Hi», «Hola») он всегда 0 при
//     верно выбранном языке — отбраковывал бы валидные приветствия;
//   - whatlanggo не определился (пусто, эмодзи, цифры, не из тройки) → 'ru'
//     (fallback: до M14 всё и так было по-русски).
func Detect(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return RU
	}
	for _, r := range text {
		if unicode.Is(unicode.Cyrillic, r) {
			return RU
		}
	}
	switch whatlanggo.DetectWithOptions(text, latinWhitelist).Lang {
	case whatlanggo.Eng:
		return EN
	case whatlanggo.Spa:
		return ES
	default:
		return RU
	}
}
