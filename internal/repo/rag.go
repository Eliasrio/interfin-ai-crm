// rag.go — репозитории M4: knowledge_chunks, conversation_summaries, rag_audit
// (SRS §7.1, §7.3, §8.4). Как и в M1, это единственная точка доступа
// бизнес-логики к таблицам: SQL векторного поиска и правило свежести summary
// живут здесь, а не в воркере.
package repo

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

// hnswEfSearch — ef_search=100 (§7.1). Параметр времени ЗАПРОСА, а не индекса:
// задаётся SET LOCAL в транзакции каждого поиска.
const hnswEfSearch = 100

// ScoredChunk — чанк базы знаний с косинусной близостью к запросу.
// Сам вектор наружу не отдаётся: ретриверу нужен только текст.
type ScoredChunk struct {
	Source     string  `gorm:"column:source"`
	ChunkIndex int     `gorm:"column:chunk_index"`
	Content    string  `gorm:"column:content"`
	Similarity float64 `gorm:"column:similarity"`
}

// KnowledgeRepo — knowledge_chunks (§7.1).
type KnowledgeRepo interface {
	// ReplaceSource атомарно заменяет все чанки документа source:
	// переиндексация не оставляет хвостов от старой версии документа.
	ReplaceSource(ctx context.Context, source string, chunks []models.KnowledgeChunk) error
	// Search — top-K чанков с cosine similarity ≥ minSimilarity,
	// по убыванию близости. Пустой срез = rag_miss (fallback — забота вызывающего).
	Search(ctx context.Context, embedding models.Vector, topK int, minSimilarity float64) ([]ScoredChunk, error)
	// CountChunks — сколько чанков в базе знаний (готовность RAG, калибровка).
	CountChunks(ctx context.Context) (int64, error)
}

// SummaryRepo — conversation_summaries (§7.3).
type SummaryRepo interface {
	// Upsert пишет сводку лида, только если она не старее существующей
	// (по message_count): проигравший гонку генератор не затирает свежее.
	Upsert(ctx context.Context, s *models.ConversationSummary) error
	// GetByLead возвращает сводку лида. ErrNotFound, если её ещё нет.
	GetByLead(ctx context.Context, leadID int64) (*models.ConversationSummary, error)
}

// RagAuditRepo — rag_audit (§8.4): след КАЖДОГО RAG-запроса, results=0 = rag_miss.
type RagAuditRepo interface {
	Create(ctx context.Context, a *models.RagAudit) error
}

// NewRAG создаёт репозитории M4 поверх того же *gorm.DB, что и repo.New.
func NewRAG(db *gorm.DB) (KnowledgeRepo, SummaryRepo, RagAuditRepo) {
	return &knowledgeRepo{db: db}, &summaryRepo{db: db}, &ragAuditRepo{db: db}
}

// --- knowledge_chunks ---

type knowledgeRepo struct{ db *gorm.DB }

func (r *knowledgeRepo) ReplaceSource(ctx context.Context, source string, chunks []models.KnowledgeChunk) error {
	if source == "" {
		return errors.New("repo: replace source: пустой source")
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("source = ?", source).
			Delete(&models.KnowledgeChunk{}).Error; err != nil {
			return fmt.Errorf("delete old chunks: %w", err)
		}
		for i := range chunks {
			chunks[i].Source = source
			chunks[i].ChunkIndex = i
		}
		if len(chunks) == 0 {
			return nil // документ опустел — просто вычищаем старые чанки
		}
		// Батчами по 100: индексация целого документа одним INSERT упёрлась бы
		// в лимиты размера запроса (SimpleProtocol интерполирует значения в SQL).
		if err := tx.CreateInBatches(chunks, 100).Error; err != nil {
			return fmt.Errorf("insert chunks: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("repo: replace source %q: %w", source, err)
	}
	return nil
}

func (r *knowledgeRepo) Search(ctx context.Context, embedding models.Vector, topK int, minSimilarity float64) ([]ScoredChunk, error) {
	if len(embedding) == 0 {
		return nil, errors.New("repo: search: пустой эмбеддинг")
	}
	if topK <= 0 {
		return nil, fmt.Errorf("repo: search: topK = %d, ожидали > 0", topK)
	}

	var out []ScoredChunk
	// Транзакция нужна ради SET LOCAL: ef_search действует до её конца
	// и не протекает в другие запросы пула.
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", hnswEfSearch)).Error; err != nil {
			return fmt.Errorf("set ef_search: %w", err)
		}
		// <=> — cosine distance pgvector; similarity = 1 - distance.
		// ORDER BY по самому оператору — обязательное условие использования
		// HNSW-индекса (фильтр по similarity индекс не использует).
		return tx.Raw(`
			SELECT source, chunk_index, content,
			       1 - (embedding <=> ?::vector) AS similarity
			FROM knowledge_chunks
			WHERE 1 - (embedding <=> ?::vector) >= ?
			ORDER BY embedding <=> ?::vector
			LIMIT ?`,
			embedding, embedding, minSimilarity, embedding, topK,
		).Scan(&out).Error
	})
	if err != nil {
		return nil, fmt.Errorf("repo: search knowledge chunks: %w", err)
	}
	return out, nil
}

func (r *knowledgeRepo) CountChunks(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.WithContext(ctx).Model(&models.KnowledgeChunk{}).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("repo: count knowledge chunks: %w", err)
	}
	return n, nil
}

// --- conversation_summaries ---

type summaryRepo struct{ db *gorm.DB }

func (r *summaryRepo) Upsert(ctx context.Context, s *models.ConversationSummary) error {
	if s.LeadID == 0 {
		return errors.New("repo: upsert summary: пустой lead_id")
	}
	// Условие в DO UPDATE — свежесть: параллельный генератор со старым
	// message_count проигрывает молча, строка остаётся более новой.
	err := r.db.WithContext(ctx).Exec(`
		INSERT INTO conversation_summaries (lead_id, content, message_count, updated_at)
		VALUES (?, ?, ?, NOW())
		ON CONFLICT (lead_id) DO UPDATE
		SET content = EXCLUDED.content,
		    message_count = EXCLUDED.message_count,
		    updated_at = NOW()
		WHERE conversation_summaries.message_count <= EXCLUDED.message_count`,
		s.LeadID, s.Content, s.MessageCount,
	).Error
	if err != nil {
		return fmt.Errorf("repo: upsert summary: %w", err)
	}
	return nil
}

func (r *summaryRepo) GetByLead(ctx context.Context, leadID int64) (*models.ConversationSummary, error) {
	var s models.ConversationSummary
	err := r.db.WithContext(ctx).
		Where("lead_id = ?", leadID).
		First(&s).Error
	if err != nil {
		return nil, wrapNotFound(err, "repo: get summary by lead")
	}
	return &s, nil
}

// --- rag_audit ---

type ragAuditRepo struct{ db *gorm.DB }

func (r *ragAuditRepo) Create(ctx context.Context, a *models.RagAudit) error {
	if err := r.db.WithContext(ctx).Create(a).Error; err != nil {
		return fmt.Errorf("repo: create rag audit: %w", err)
	}
	return nil
}
