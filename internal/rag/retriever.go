// retriever.go — retrieval-половина RAG (M4-3/4, §7.1): эмбеддинг запроса →
// top-K по cosine ≥ threshold → аудит. 0 результатов — НЕ ошибка: воркер
// отвечает без RAG (fallback), а промах фиксируется в rag_audit (results=0).
package rag

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/embeddings"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// Retriever ищет релевантные чанки базы знаний под запрос лида.
type Retriever struct {
	embedder  Embedder
	knowledge repo.KnowledgeRepo
	audit     repo.RagAuditRepo
	threshold float64 // rag.cosine_threshold (§7.1: 0.78)
	topK      int     // rag.top_k (§7.1: 5)
	log       *slog.Logger
}

// NewRetriever читает порог и top_k из конфига (критерий приёмки M4) —
// никаких зашитых констант.
func NewRetriever(
	cfg config.RAGConfig,
	embedder Embedder,
	knowledge repo.KnowledgeRepo,
	audit repo.RagAuditRepo,
	log *slog.Logger,
) (*Retriever, error) {
	if cfg.CosineThreshold <= 0 || cfg.CosineThreshold >= 1 {
		return nil, fmt.Errorf("rag: cosine_threshold вне (0,1): %v", cfg.CosineThreshold)
	}
	if cfg.TopK <= 0 {
		return nil, fmt.Errorf("rag: top_k должен быть > 0: %d", cfg.TopK)
	}
	return &Retriever{
		embedder:  embedder,
		knowledge: knowledge,
		audit:     audit,
		threshold: cfg.CosineThreshold,
		topK:      cfg.TopK,
		log:       log,
	}, nil
}

// Retrieve возвращает top-K чанков с cosine ≥ threshold (может быть пусто —
// rag_miss). Каждый вызов оставляет след в rag_audit; неудача записи аудита
// диалог не роняет (ответ лиду важнее строки аудита) — только warn в лог.
// Ошибка Voyage/БД возвращается наверх: это транзиент, его лечит ретрай
// Asynq, а не молчаливый ответ без знаний.
func (r *Retriever) Retrieve(ctx context.Context, leadID int64, query string) ([]repo.ScoredChunk, error) {
	vecs, err := r.embedder.Embed(ctx, []string{query}, embeddings.InputQuery)
	if err != nil {
		return nil, fmt.Errorf("rag: эмбеддинг запроса: %w", err)
	}

	chunks, err := r.knowledge.Search(ctx, models.Vector(vecs[0]), r.topK, r.threshold)
	if err != nil {
		return nil, fmt.Errorf("rag: поиск: %w", err)
	}

	rec := &models.RagAudit{
		LeadID:    &leadID,
		QueryText: query,
		Results:   len(chunks),
		Threshold: r.threshold,
	}
	if err := r.audit.Create(ctx, rec); err != nil {
		r.log.Warn("rag: запись rag_audit не удалась", "lead_id", leadID, "error", err)
	}

	if len(chunks) == 0 {
		r.log.Info("rag: rag_miss, ответ пойдёт без RAG (fallback §7.1)",
			"lead_id", leadID, "threshold", r.threshold)
	}
	return chunks, nil
}
