// scripts/rag_calibrate — калибровка порога cosine similarity (M4-7, SRS §7.1).
//
// Считает recall@K на тестовой выборке «запрос → ожидаемый документ» против
// уже проиндексированной базы знаний (cmd/index-kb). Если recall@5 < 90% —
// рекомендует снизить rag.cosine_threshold до 0.72 (§7.1; паспортное значение
// 0.78 давало recall@5 = 94% на 200 Q&A).
//
// Использование:
//
//	go run ./scripts/rag_calibrate -testset ./docs/kb/testset.json [-threshold 0.78] [-topk 5]
//
// Формат testset.json:
//
//	[{"query": "какие тарифы?", "expected_source": "faq.md"}, ...]
//
// Обязательные переменные: POSTGRES_DSN, VOYAGE_API_KEY.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/embeddings"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

const (
	// targetRecall — приёмочный порог §7.1: ниже — порог пора снижать.
	targetRecall = 0.90
	// recommendedThreshold — рекомендация §7.1 при recall@5 < 90%.
	recommendedThreshold = 0.72
)

// testCase — один вопрос выборки и документ, который обязан найтись.
type testCase struct {
	Query          string `json:"query"`
	ExpectedSource string `json:"expected_source"`
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("rag_calibrate failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	testsetPath := flag.String("testset", "./docs/kb/testset.json", "путь к тестовой выборке (JSON)")
	threshold := flag.Float64("threshold", 0.78, "проверяемый cosine threshold (rag.cosine_threshold)")
	topK := flag.Int("topk", 5, "top-K retrieval (rag.top_k)")
	flag.Parse()

	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return errors.New("обязательная переменная POSTGRES_DSN не задана")
	}
	voyageKey := os.Getenv("VOYAGE_API_KEY")
	if voyageKey == "" {
		return errors.New("обязательная переменная VOYAGE_API_KEY не задана")
	}

	raw, err := os.ReadFile(*testsetPath)
	if err != nil {
		return fmt.Errorf("тестовая выборка: %w", err)
	}
	var cases []testCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		return fmt.Errorf("тестовая выборка %s: разбор JSON: %w", *testsetPath, err)
	}
	if len(cases) == 0 {
		return fmt.Errorf("тестовая выборка %s пуста", *testsetPath)
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

	knowledge, _, _ := repo.NewRAG(gormDB)
	ctx := context.Background()

	if n, err := knowledge.CountChunks(ctx); err != nil {
		return err
	} else if n == 0 {
		return errors.New("база знаний пуста — сначала go run ./cmd/index-kb")
	}

	embedder, err := embeddings.New(config.EmbeddingsConfig{
		Provider:   "voyage",
		APIKey:     voyageKey,
		Model:      "voyage-3", // §7.1 / CLAUDE.md §3
		Dimensions: 1024,
	})
	if err != nil {
		return err
	}

	// Все запросы эмбеддятся одним батчем (клиент сам порежет по лимиту API).
	queries := make([]string, len(cases))
	for i, c := range cases {
		queries[i] = c.Query
	}
	vecs, err := embedder.Embed(ctx, queries, embeddings.InputQuery)
	if err != nil {
		return err
	}

	hits := 0
	for i, c := range cases {
		found, err := knowledge.Search(ctx, models.Vector(vecs[i]), *topK, *threshold)
		if err != nil {
			return err
		}
		hit := false
		for _, chunk := range found {
			if chunk.Source == c.ExpectedSource {
				hit = true
				break
			}
		}
		if hit {
			hits++
			continue
		}
		log.Warn("промах", "query", c.Query, "expected_source", c.ExpectedSource, "results", len(found))
	}

	recall := float64(hits) / float64(len(cases))
	log.Info("recall@K посчитан",
		"recall", fmt.Sprintf("%.1f%%", recall*100),
		"hits", hits, "cases", len(cases),
		"threshold", *threshold, "top_k", *topK)

	if recall < targetRecall {
		log.Warn(fmt.Sprintf(
			"recall@%d = %.1f%% < %.0f%% — рекомендация §7.1: снизить rag.cosine_threshold до %.2f (config.yaml)",
			*topK, recall*100, targetRecall*100, recommendedThreshold))
		return nil
	}
	log.Info(fmt.Sprintf("recall@%d ≥ %.0f%% — порог %.2f оставляем", *topK, targetRecall*100, *threshold))
	return nil
}
