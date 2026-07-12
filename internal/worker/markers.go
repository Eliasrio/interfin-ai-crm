// markers.go — общий парсер маркер-протокола панели Эммы (EP-04, ТЗ §3):
// Эмма управляет действиями воркера метками в конце ответа — {{file:N}}
// «отправь файл N» (EP-04) и {{handoff}} «передай менеджеру» (EP-05).
//
// Контракт наружу (EP-05): clean/fileIDs/handoff не меняются. Маркеры
// вырезаются ДО CreateOutbound — в БД, событие M12 и историю диалога уходит
// чистый текст (история не должна учить Эмму подражать маркерам, ТЗ §3).
package worker

import (
	"regexp"
	"strconv"
	"strings"
)

// markerHandoff — запрос передачи менеджеру (обработка — EP-05, здесь
// только вырезается и логируется вызывающим).
const markerHandoff = "{{handoff}}"

var (
	// markerRe — всё маркер-образное: {{...}} без вложенных скобок. Шире
	// валидных маркеров намеренно: битый/чужой маркер тоже не должен
	// дойти до клиента (ТЗ §3) — он вырезается и попадает в unknown.
	markerRe = regexp.MustCompile(`\{\{[^{}]*\}\}`)
	// fileMarkerRe — валидный файловый маркер {{file:N}}.
	fileMarkerRe = regexp.MustCompile(`^\{\{file:(\d+)\}\}$`)

	// Схлопывание следов вырезания: двойные пробелы внутри строки, пробелы
	// перед переводом строки, тройные+ пустые строки.
	spaceRunRe     = regexp.MustCompile(`[ \t]{2,}`)
	trailingWSRe   = regexp.MustCompile(`[ \t]+\n`)
	blankLineRunRe = regexp.MustCompile(`\n{3,}`)
)

// ParseMarkers вырезает из ответа Эммы ВСЕ маркеры и возвращает чистый
// текст, id файлов на отправку (в порядке появления, без дублей), флаг
// handoff и нераспознанные маркеры ({{file:abc}}, {{чужое}}) — вызывающий
// обязан их залогировать. Контракт EP-05 — первые три значения.
func ParseMarkers(text string) (clean string, fileIDs []int64, handoff bool, unknown []string) {
	seen := map[int64]bool{}
	clean = markerRe.ReplaceAllStringFunc(text, func(m string) string {
		switch {
		case m == markerHandoff:
			handoff = true
		case fileMarkerRe.MatchString(m):
			id, err := strconv.ParseInt(fileMarkerRe.FindStringSubmatch(m)[1], 10, 64)
			if err != nil {
				// \d+ длиннее int64 — маркер битый.
				unknown = append(unknown, m)
				break
			}
			if !seen[id] {
				seen[id] = true
				fileIDs = append(fileIDs, id)
			}
		default:
			unknown = append(unknown, m)
		}
		return ""
	})

	clean = trailingWSRe.ReplaceAllString(clean, "\n")
	clean = spaceRunRe.ReplaceAllString(clean, " ")
	clean = blankLineRunRe.ReplaceAllString(clean, "\n\n")
	clean = strings.TrimSpace(clean)
	return clean, fileIDs, handoff, unknown
}
