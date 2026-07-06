// cmd/index-kb — индексация базы знаний RAG (M4-2, SRS §7.1):
// каталог .md/.txt-документов → чанкинг → Voyage-эмбеддинги → pgvector.
//
// Использование:
//
//	go run ./cmd/index-kb -dir ./docs/kb
//
// Обязательные переменные: POSTGRES_DSN, VOYAGE_API_KEY (как у cmd/migrate —
// CLI читает окружение напрямую, без полного config.yaml с телеграм-секретами).
// Документ = файл; source = имя файла; переиндексация заменяет чанки source
// атомарно. Прод-запуск — вручную при обновлении базы знаний, как миграции.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/embeddings"
	"github.com/interfin/interfin-ai-crm/internal/rag"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("index-kb failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	dir := flag.String("dir", "./docs/kb", "каталог с документами базы знаний (.md, .txt)")
	flag.Parse()

	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return errors.New("обязательная переменная POSTGRES_DSN не задана")
	}
	voyageKey := os.Getenv("VOYAGE_API_KEY")
	if voyageKey == "" {
		return errors.New("обязательная переменная VOYAGE_API_KEY не задана (AQ²-2: Voyage, не OpenAI)")
	}

	gormDB, err := db.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 4})
	if err != nil {
		return err
	}
	sqlDB, err := gormDB.DB()
	if err != nil {
		return fmt.Errorf("gorm sql db: %w", err)
	}
	defer sqlDB.Close()

	embedder, err := embeddings.New(config.EmbeddingsConfig{
		Provider:   "voyage",
		APIKey:     voyageKey,
		Model:      "voyage-3", // §7.1 / CLAUDE.md §3
		Dimensions: 1024,
	})
	if err != nil {
		return err
	}

	knowledge, _, _ := repo.NewRAG(gormDB)
	indexer := rag.NewIndexer(embedder, knowledge, log)

	files, err := knowledgeFiles(*dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("в %s нет документов .md/.txt — индексировать нечего", *dir)
	}

	ctx := context.Background()
	totalChunks := 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("чтение %s: %w", path, err)
		}
		// source — имя файла: стабильный ключ переиндексации (§7.1).
		n, err := indexer.IndexDocument(ctx, filepath.Base(path), string(raw))
		if err != nil {
			return err
		}
		totalChunks += n
	}

	total, err := knowledge.CountChunks(ctx)
	if err != nil {
		return err
	}
	log.Info("база знаний проиндексирована",
		"documents", len(files), "chunks_written", totalChunks, "chunks_total", total)
	return nil
}

// knowledgeFiles — .md/.txt файлы каталога dir (без рекурсии, по алфавиту).
func knowledgeFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("каталог базы знаний %s: %w", dir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".md", ".txt":
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}
