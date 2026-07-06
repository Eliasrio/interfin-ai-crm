package schemalint

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplySQL_CreateTable(t *testing.T) {
	schema := Schema{}
	sql := `
-- комментарий с ловушкой: fake_column BIGINT
CREATE TABLE leads (
  id                BIGSERIAL PRIMARY KEY,
  telegram_user_id  BIGINT        NOT NULL UNIQUE,
  amount            NUMERIC(20,8),                 -- вложенные скобки с запятой
  direction         VARCHAR(8) CHECK (direction IN ('a','b')),
  CONSTRAINT uq_x UNIQUE (telegram_user_id),
  PRIMARY KEY (id)
);
CREATE INDEX idx_leads_x ON leads(telegram_user_id);
`
	if err := applySQL(schema, sql); err != nil {
		t.Fatal(err)
	}
	want := []string{"id", "telegram_user_id", "amount", "direction"}
	got := schema["leads"]
	if len(got) != len(want) {
		t.Fatalf("колонки: получили %v, ждали %v", got, want)
	}
	for _, c := range want {
		if !got[c] {
			t.Errorf("нет колонки %q", c)
		}
	}
	if got["fake_column"] {
		t.Error("колонка из SQL-комментария не должна попадать в схему")
	}
}

func TestApplySQL_AlterAndDrop(t *testing.T) {
	schema := Schema{}
	steps := []string{
		`CREATE TABLE t ( id BIGSERIAL PRIMARY KEY );`,
		`ALTER TABLE t ADD COLUMN extra VARCHAR(10);`,
		`ALTER TABLE t DROP COLUMN extra;`,
		`CREATE TABLE gone ( id BIGSERIAL PRIMARY KEY );`,
		`DROP TABLE gone;`,
	}
	for _, s := range steps {
		if err := applySQL(schema, s); err != nil {
			t.Fatal(err)
		}
	}
	if schema["t"]["extra"] {
		t.Error("DROP COLUMN не применился")
	}
	if _, ok := schema["gone"]; ok {
		t.Error("DROP TABLE не применился")
	}
}

func TestCheck(t *testing.T) {
	have := Schema{"leads": {"id": true, "name": true}}

	// Модель ссылается на колонку, которой нет — ошибка (AQ²-1).
	res := Check(Schema{"leads": {"id": true, "pending_task": true}}, have)
	if len(res.Errors) != 1 {
		t.Fatalf("ждали 1 ошибку, получили %v", res.Errors)
	}

	// Колонка без поля в модели — только предупреждение.
	res = Check(Schema{"leads": {"id": true}}, have)
	if len(res.Errors) != 0 || len(res.Warnings) != 1 {
		t.Fatalf("ждали 0 ошибок и 1 warning, получили %v / %v", res.Errors, res.Warnings)
	}

	// Таблица не создаётся миграциями — ошибка.
	res = Check(Schema{"ghost": {"id": true}}, have)
	if len(res.Errors) != 1 {
		t.Fatalf("ждали 1 ошибку про таблицу, получили %v", res.Errors)
	}
}

func TestModelColumns_RequiresExplicitTag(t *testing.T) {
	_, errs := ModelColumns([]interface{}{tagless{}})
	if len(errs) != 1 {
		t.Fatalf("ждали 1 ошибку про отсутствующий тег, получили %v", errs)
	}
}

type tagless struct {
	ID   int64 `gorm:"column:id"`
	Name string
}

func (tagless) TableName() string { return "tagless" }

func TestModelColumns_NoTableName(t *testing.T) {
	type anon struct {
		ID int64 `gorm:"column:id"`
	}
	_, errs := ModelColumns([]interface{}{anon{}})
	if len(errs) != 1 {
		t.Fatalf("ждали 1 ошибку про TableName, получили %v", errs)
	}
}

func TestParseMigrations_RealDir(t *testing.T) {
	// Прогон по настоящим миграциям репо: schema-lint обязан их разбирать.
	dir := filepath.Join("..", "..", "migrations")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("migrations/ недоступен")
	}
	schema, err := ParseMigrations(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"leads", "messages", "payment_events", "rag_audit", "lgpd_audit"} {
		if _, ok := schema[tbl]; !ok {
			t.Errorf("в миграциях не найдена таблица %q", tbl)
		}
	}
	if !schema["leads"]["pending_task"] {
		t.Error("leads.pending_task не найдена (AQ²-1!)")
	}
}
