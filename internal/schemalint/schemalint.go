// Package schemalint — ядро CI-проверки single-source-of-truth схемы
// (AQ²-fix #1, CLAUDE.md §4.1, задача M1-5).
//
// Схема из миграций (CREATE TABLE / ALTER TABLE ADD COLUMN в *.up.sql)
// сверяется с GORM-моделями:
//
//	ОШИБКА  — поле модели ссылается на колонку, которой нет в миграциях
//	          (прецедент: забытое pending_task);
//	ОШИБКА  — у персистентного поля нет явного тега gorm:"column:...";
//	ОШИБКА  — у модели нет метода TableName() или таблица не создаётся
//	          миграциями;
//	WARNING — колонка есть в миграциях, но не отражена ни одной моделью
//	          (не валит CI: схема может опережать код).
package schemalint

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// Schema: имя таблицы → множество колонок (всё в нижнем регистре).
type Schema map[string]map[string]bool

var (
	reComment     = regexp.MustCompile(`--[^\n]*`)
	reCreateTable = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"?(\w+)"?\s*\(`)
	reAddColumn   = regexp.MustCompile(`(?is)ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?"?(\w+)"?\s+ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?"?(\w+)"?`)
	reDropColumn  = regexp.MustCompile(`(?is)ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?"?(\w+)"?\s+DROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?"?(\w+)"?`)
	reDropTable   = regexp.MustCompile(`(?is)DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?"?(\w+)"?`)
)

// Слова, с которых начинается табличный констрейнт, а не колонка.
var constraintKeywords = map[string]bool{
	"primary": true, "foreign": true, "unique": true, "check": true,
	"constraint": true, "exclude": true, "like": true,
}

// ParseMigrations читает dir/*.up.sql в лексикографическом порядке (то есть в
// порядке применения) и строит итоговую схему.
func ParseMigrations(dir string) (Schema, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		return nil, fmt.Errorf("schemalint: glob %s: %w", dir, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("schemalint: в %s нет *.up.sql", dir)
	}
	sort.Strings(files)

	schema := Schema{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("schemalint: read %s: %w", f, err)
		}
		if err := applySQL(schema, string(raw)); err != nil {
			return nil, fmt.Errorf("schemalint: %s: %w", filepath.Base(f), err)
		}
	}
	return schema, nil
}

// applySQL накатывает на schema все DDL-выражения одного файла.
func applySQL(schema Schema, sql string) error {
	sql = reComment.ReplaceAllString(sql, "")

	for _, m := range reCreateTable.FindAllStringSubmatchIndex(sql, -1) {
		table := strings.ToLower(sql[m[2]:m[3]])
		body, err := parenBody(sql[m[1]-1:]) // m[1] — позиция сразу после '('
		if err != nil {
			return fmt.Errorf("CREATE TABLE %s: %w", table, err)
		}
		cols, err := parseColumns(body)
		if err != nil {
			return fmt.Errorf("CREATE TABLE %s: %w", table, err)
		}
		if schema[table] == nil {
			schema[table] = map[string]bool{}
		}
		for _, c := range cols {
			schema[table][c] = true
		}
	}

	for _, m := range reAddColumn.FindAllStringSubmatch(sql, -1) {
		table, col := strings.ToLower(m[1]), strings.ToLower(m[2])
		if schema[table] == nil {
			schema[table] = map[string]bool{}
		}
		schema[table][col] = true
	}
	for _, m := range reDropColumn.FindAllStringSubmatch(sql, -1) {
		delete(schema[strings.ToLower(m[1])], strings.ToLower(m[2]))
	}
	for _, m := range reDropTable.FindAllStringSubmatch(sql, -1) {
		delete(schema, strings.ToLower(m[1]))
	}
	return nil
}

// parenBody возвращает содержимое скобок, начинающихся с s[0]=='(',
// с учётом вложенности (NUMERIC(20,8), CHECK (...)).
func parenBody(s string) (string, error) {
	if s == "" || s[0] != '(' {
		return "", fmt.Errorf("ожидалась '('")
	}
	depth := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[1:i], nil
			}
		}
	}
	return "", fmt.Errorf("непарная '('")
}

// parseColumns разбивает тело CREATE TABLE по запятым верхнего уровня и
// возвращает имена колонок, пропуская табличные констрейнты.
func parseColumns(body string) ([]string, error) {
	var cols []string
	for _, def := range splitTopLevel(body) {
		def = strings.TrimSpace(def)
		if def == "" {
			continue
		}
		first := strings.ToLower(strings.Trim(strings.Fields(def)[0], `"`))
		if constraintKeywords[first] {
			continue
		}
		if !regexp.MustCompile(`^[a-z_][a-z0-9_]*$`).MatchString(first) {
			return nil, fmt.Errorf("не удалось разобрать определение колонки: %q", def)
		}
		cols = append(cols, first)
	}
	return cols, nil
}

// splitTopLevel режет строку по запятым нулевой глубины скобок.
func splitTopLevel(s string) []string {
	var parts []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

// ModelColumns извлекает через reflect карту таблица→колонки из GORM-моделей
// (реестр models.All()). Ошибка, если у модели нет TableName() или у
// персистентного поля нет явного тега column.
func ModelColumns(regs []interface{}) (Schema, []error) {
	schema := Schema{}
	var errs []error

	type tabler interface{ TableName() string }

	for _, reg := range regs {
		t := reflect.TypeOf(reg)
		tn, ok := reg.(tabler)
		if !ok {
			errs = append(errs, fmt.Errorf("модель %s: нет метода TableName()", t.Name()))
			continue
		}
		table := strings.ToLower(tn.TableName())
		if schema[table] == nil {
			schema[table] = map[string]bool{}
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := f.Tag.Get("gorm")
			if tag == "-" {
				continue
			}
			col := gormColumn(tag)
			if col == "" {
				errs = append(errs, fmt.Errorf(
					"модель %s: поле %s без явного тега gorm:\"column:...\" (правило пакета models)",
					t.Name(), f.Name))
				continue
			}
			schema[table][strings.ToLower(col)] = true
		}
	}
	return schema, errs
}

// gormColumn достаёт column:<имя> из gorm-тега.
func gormColumn(tag string) string {
	for _, part := range strings.Split(tag, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(strings.ToLower(part), "column:") {
			return strings.TrimSpace(part[len("column:"):])
		}
	}
	return ""
}

// Result — итог сверки.
type Result struct {
	Errors   []string // модель → схема: валят CI
	Warnings []string // схема → модель: информируют
}

// Check сверяет колонки моделей (want) с колонками миграций (have).
func Check(want, have Schema) Result {
	var res Result

	for table, cols := range want {
		haveCols, ok := have[table]
		if !ok {
			res.Errors = append(res.Errors, fmt.Sprintf(
				"таблица %q используется моделью, но не создаётся ни одной миграцией", table))
			continue
		}
		for col := range cols {
			if !haveCols[col] {
				res.Errors = append(res.Errors, fmt.Sprintf(
					"%s.%s: колонка используется моделью, но отсутствует в migrations/ (AQ²-1)", table, col))
			}
		}
	}

	for table, cols := range have {
		wantCols, ok := want[table]
		if !ok {
			// Таблица без модели — нормально (schema_migrations, будущие эпики).
			continue
		}
		for col := range cols {
			if !wantCols[col] {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"%s.%s: колонка есть в миграциях, но не отражена в модели", table, col))
			}
		}
	}

	sort.Strings(res.Errors)
	sort.Strings(res.Warnings)
	return res
}
