// indexer.go — индексация базы знаний (M4-2, §7.1):
// документ → чанки → Voyage-эмбеддинги → pgvector (replace по source).
package rag

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/interfin/interfin-ai-crm/internal/embeddings"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// Embedder — контракт на Voyage-клиент (в тестах фейк).
type Embedder interface {
	Embed(ctx context.Context, texts []string, inputType string) ([][]float32, error)
}

// Indexer пишет документы базы знаний в pgvector.
type Indexer struct {
	embedder      Embedder
	knowledge     repo.KnowledgeRepo
	maxChunkChars int
	log           *slog.Logger
}

func NewIndexer(embedder Embedder, knowledge repo.KnowledgeRepo, log *slog.Logger) *Indexer {
	return &Indexer{
		embedder:      embedder,
		knowledge:     knowledge,
		maxChunkChars: DefaultMaxChunkChars,
		log:           log,
	}
}

// IndexDocument (пере)индексирует документ source: старые чанки source
// заменяются новыми атомарно. Возвращает число записанных чанков.
func (ix *Indexer) IndexDocument(ctx context.Context, source, text string) (int, error) {
	pieces := SplitIntoChunks(text, ix.maxChunkChars)
	if len(pieces) == 0 {
		// Документ опустел — вычищаем его чанки из индекса.
		if err := ix.knowledge.ReplaceSource(ctx, source, nil); err != nil {
			return 0, fmt.Errorf("rag: индексация %s: %w", source, err)
		}
		ix.log.Warn("rag: документ пуст, чанки source удалены", "source", source)
		return 0, nil
	}

	vecs, err := ix.embedder.Embed(ctx, pieces, embeddings.InputDocument)
	if err != nil {
		return 0, fmt.Errorf("rag: эмбеддинг %s: %w", source, err)
	}

	chunks := make([]models.KnowledgeChunk, len(pieces))
	for i, content := range pieces {
		chunks[i] = models.KnowledgeChunk{Content: content, Embedding: vecs[i]}
	}
	if err := ix.knowledge.ReplaceSource(ctx, source, chunks); err != nil {
		return 0, fmt.Errorf("rag: индексация %s: %w", source, err)
	}
	ix.log.Info("rag: документ проиндексирован", "source", source, "chunks", len(chunks))
	return len(chunks), nil
}
