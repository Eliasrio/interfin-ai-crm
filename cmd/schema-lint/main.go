// cmd/schema-lint — CI-проверка single-source-of-truth схемы (AQ²-fix #1).
//
//	go run ./cmd/schema-lint [-migrations ./migrations]
//
// Exit code 1, если Go-модель ссылается на колонку, которой нет в миграциях.
// Логика — в internal/schemalint, реестр моделей — models.All().
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/schemalint"
)

func main() {
	dir := flag.String("migrations", "./migrations", "каталог с NNNN_*.up.sql")
	flag.Parse()

	have, err := schemalint.ParseMigrations(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "schema-lint:", err)
		os.Exit(1)
	}

	want, modelErrs := schemalint.ModelColumns(models.All())
	res := schemalint.Check(want, have)

	for _, w := range res.Warnings {
		fmt.Fprintln(os.Stderr, "schema-lint: WARNING:", w)
	}
	failed := false
	for _, e := range modelErrs {
		fmt.Fprintln(os.Stderr, "schema-lint: ОШИБКА:", e)
		failed = true
	}
	for _, e := range res.Errors {
		fmt.Fprintln(os.Stderr, "schema-lint: ОШИБКА:", e)
		failed = true
	}
	if failed {
		os.Exit(1)
	}
	fmt.Printf("schema-lint: OK — %d таблиц, модели соответствуют миграциям\n", len(want))
}
