package repo

import (
	"context"
	"errors"
	"math"
	"os"
	"testing"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/models"
)

// Интеграционные тесты M4 против реального PostgreSQL+pgvector
// (образ pgvector/pgvector:pg16 — docker-compose / CI). Запуск как у M1:
// POSTGRES_TEST_DSN=... go test ./internal/repo; без переменной — skip.

const testDims = 1024 // vector(1024) — размерность фиксирована схемой (§7.1)

func testRAGRepos(t *testing.T) (KnowledgeRepo, SummaryRepo, RagAuditRepo, LeadRepo) {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN не задан — интеграционный тест пропущен")
	}
	gdb, err := db.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	err = gdb.Exec(`TRUNCATE leads, knowledge_chunks, conversation_summaries, rag_audit RESTART IDENTITY CASCADE`).Error
	if err != nil {
		t.Fatalf("truncate: %v (схема накатана? go run ./cmd/migrate up)", err)
	}
	kn, sum, audit := NewRAG(gdb)
	leads, _, _ := New(gdb)
	return kn, sum, audit, leads
}

// basisVec — 1024-мерный вектор с заданными компонентами в начале,
// остальное нули. Косинусные близости таких векторов считаются точно.
func basisVec(components ...float32) models.Vector {
	v := make(models.Vector, testDims)
	copy(v, components)
	return v
}

func chunk(content string, emb models.Vector) models.KnowledgeChunk {
	return models.KnowledgeChunk{Content: content, Embedding: emb}
}

func TestKnowledgeSearch_ThresholdOrderAndTopK(t *testing.T) {
	kn, _, _, _ := testRAGRepos(t)
	ctx := context.Background()

	// Три чанка по осям e0, e1, e2.
	err := kn.ReplaceSource(ctx, "faq.md", []models.KnowledgeChunk{
		chunk("про тарифы", basisVec(1)),
		chunk("про вывод средств", basisVec(0, 1)),
		chunk("про офис в Бузиосе", basisVec(0, 0, 1)),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Запрос 0.8·e0 + 0.6·e1: cos к «тарифам» = 0.8, к «выводу» = 0.6, к «офису» = 0.
	query := basisVec(0.8, 0.6)

	got, err := kn.Search(ctx, query, 5, 0.78)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "про тарифы" {
		t.Fatalf("threshold 0.78: ожидали только «про тарифы», получили %+v", got)
	}
	if math.Abs(got[0].Similarity-0.8) > 1e-4 {
		t.Errorf("similarity = %v, ожидали ≈0.8", got[0].Similarity)
	}

	// Порог ниже — входят два, по убыванию близости.
	got, err = kn.Search(ctx, query, 5, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Content != "про тарифы" || got[1].Content != "про вывод средств" {
		t.Fatalf("threshold 0.5: ожидали [тарифы, вывод], получили %+v", got)
	}

	// top_k=1 режет выдачу.
	got, err = kn.Search(ctx, query, 1, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("topK=1: получили %d результатов", len(got))
	}

	// Совсем непохожий запрос → rag_miss (пустая выдача, не ошибка).
	got, err = kn.Search(ctx, basisVec(0, 0, 0, 1), 5, 0.78)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ожидали 0 результатов, получили %+v", got)
	}
}

func TestKnowledgeReplaceSource_NoLeftovers(t *testing.T) {
	kn, _, _, _ := testRAGRepos(t)
	ctx := context.Background()

	err := kn.ReplaceSource(ctx, "doc.md", []models.KnowledgeChunk{
		chunk("старый 1", basisVec(1)),
		chunk("старый 2", basisVec(0, 1)),
		chunk("старый 3", basisVec(0, 0, 1)),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Переиндексация той же source более коротким документом.
	err = kn.ReplaceSource(ctx, "doc.md", []models.KnowledgeChunk{
		chunk("новый 1", basisVec(1)),
	})
	if err != nil {
		t.Fatal(err)
	}

	n, err := kn.CountChunks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("после переиндексации %d чанков, ожидали 1 (без хвостов)", n)
	}
	got, err := kn.Search(ctx, basisVec(1), 5, 0.9)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "новый 1" || got[0].ChunkIndex != 0 {
		t.Fatalf("ожидали свежий чанк, получили %+v", got)
	}
}

func TestSummaryUpsert_FreshnessWins(t *testing.T) {
	_, sum, _, leads := testRAGRepos(t)
	ctx := context.Background()

	lead := &models.Lead{TelegramUserID: 42}
	if err := leads.Create(ctx, lead); err != nil {
		t.Fatal(err)
	}

	if _, err := sum.GetByLead(ctx, lead.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("до записи ожидали ErrNotFound, получили %v", err)
	}

	up := func(content string, mc int) {
		t.Helper()
		err := sum.Upsert(ctx, &models.ConversationSummary{
			LeadID: lead.ID, Content: content, MessageCount: mc,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	up("сводка после 15", 15)
	up("сводка после 30", 30)
	// Отставший генератор (гонка IQ-5) не должен затереть свежую сводку.
	up("устаревшая сводка", 15)

	got, err := sum.GetByLead(ctx, lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "сводка после 30" || got.MessageCount != 30 {
		t.Fatalf("ожидали свежую сводку (30), получили %+v", got)
	}
}

func TestRagAuditCreate(t *testing.T) {
	_, _, audit, _ := testRAGRepos(t)
	ctx := context.Background()

	leadID := int64(7)
	rec := &models.RagAudit{LeadID: &leadID, QueryText: "какие тарифы?", Results: 0, Threshold: 0.78}
	if err := audit.Create(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if rec.ID == 0 {
		t.Fatal("rag_audit: ID не заполнен после вставки")
	}
}
