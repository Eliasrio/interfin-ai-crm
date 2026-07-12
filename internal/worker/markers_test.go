// Тесты парсера маркер-протокола (EP-04): вырезание {{file:N}}/{{handoff}},
// битые маркеры, схлопывание пробелов, дедупликация id.
package worker

import (
	"reflect"
	"testing"
)

func TestParseMarkers(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantClean   string
		wantFiles   []int64
		wantHandoff bool
		wantUnknown []string
	}{
		{
			name:      "без маркеров — текст не тронут",
			in:        "Добрый день! Чем могу помочь?",
			wantClean: "Добрый день! Чем могу помочь?",
		},
		{
			name:      "файловый маркер в конце",
			in:        "Вот наш прайс. {{file:3}}",
			wantClean: "Вот наш прайс.",
			wantFiles: []int64{3},
		},
		{
			name:      "несколько маркеров",
			in:        "Отправляю документы. {{file:3}} {{file:5}}",
			wantClean: "Отправляю документы.",
			wantFiles: []int64{3, 5},
		},
		{
			name:      "дубль id схлопывается",
			in:        "Прайс: {{file:3}} {{file:3}}",
			wantClean: "Прайс:",
			wantFiles: []int64{3},
		},
		{
			name:        "handoff вырезается и поднимает флаг",
			in:          "Сейчас позову менеджера. {{handoff}}",
			wantClean:   "Сейчас позову менеджера.",
			wantHandoff: true,
		},
		{
			name:        "handoff + файл",
			in:          "Вот прайс, зову менеджера. {{file:7}}\n{{handoff}}",
			wantClean:   "Вот прайс, зову менеджера.",
			wantFiles:   []int64{7},
			wantHandoff: true,
		},
		{
			name:        "битый маркер вырезается в unknown",
			in:          "Смотрите файл {{file:abc}} тут.",
			wantClean:   "Смотрите файл тут.",
			wantUnknown: []string{"{{file:abc}}"},
		},
		{
			name:        "чужой маркер вырезается в unknown",
			in:          "Ответ {{something}} готов.",
			wantClean:   "Ответ готов.",
			wantUnknown: []string{"{{something}}"},
		},
		{
			name:      "маркер в середине — двойной пробел схлопнут",
			in:        "Прайс {{file:2}} во вложении.",
			wantClean: "Прайс во вложении.",
			wantFiles: []int64{2},
		},
		{
			name:      "пустые хвосты и лишние пустые строки схлопнуты",
			in:        "Первый абзац. {{file:1}}\n\n{{file:2}}\n\nВторой абзац. {{file:3}}\n\n",
			wantClean: "Первый абзац.\n\nВторой абзац.",
			wantFiles: []int64{1, 2, 3},
		},
		{
			name:      "ответ из одного маркера — clean пуст",
			in:        "{{file:4}}",
			wantClean: "",
			wantFiles: []int64{4},
		},
		{
			name:        "id длиннее int64 — битый маркер",
			in:          "Файл {{file:99999999999999999999999}} готов.",
			wantClean:   "Файл готов.",
			wantUnknown: []string{"{{file:99999999999999999999999}}"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clean, files, handoff, unknown := ParseMarkers(tt.in)
			if clean != tt.wantClean {
				t.Errorf("clean = %q, ждали %q", clean, tt.wantClean)
			}
			if !reflect.DeepEqual(files, tt.wantFiles) {
				t.Errorf("fileIDs = %v, ждали %v", files, tt.wantFiles)
			}
			if handoff != tt.wantHandoff {
				t.Errorf("handoff = %v, ждали %v", handoff, tt.wantHandoff)
			}
			if !reflect.DeepEqual(unknown, tt.wantUnknown) {
				t.Errorf("unknown = %v, ждали %v", unknown, tt.wantUnknown)
			}
		})
	}
}
