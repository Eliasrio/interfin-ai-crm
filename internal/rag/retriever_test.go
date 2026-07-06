package rag

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/embeddings"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// --- фейки ---

type fakeEmbedder struct {
	gotTexts     []string
	gotInputType string
	vec          []float32
	err          error
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string, inputType string) ([][]float32, error) {
	f.gotTexts = texts
	f.gotInputType = inputType
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = f.vec
	}
	return out, nil
}

type fakeKnowledge struct {
	gotTopK   int
	gotMinSim float64
	results   []repo.ScoredChunk
	err       error

	replaced map[string][]models.KnowledgeChunk
}

func (f *fakeKnowledge) ReplaceSource(_ context.Context, source string, chunks []models.KnowledgeChunk) error {
	if f.replaced == nil {
		f.replaced = map[string][]models.KnowledgeChunk{}
	}
	f.replaced[source] = chunks
	return f.err
}

func (f *fakeKnowledge) Search(_ context.Context, _ models.Vector, topK int, minSim float64) ([]repo.ScoredChunk, error) {
	f.gotTopK = topK
	f.gotMinSim = minSim
	return f.results, f.err
}

func (f *fakeKnowledge) CountChunks(context.Context) (int64, error) { return 0, nil }

type fakeAudit struct {
	records []models.RagAudit
	err     error
}

func (f *fakeAudit) Create(_ context.Context, a *models.RagAudit) error {
	if f.err != nil {
		return f.err
	}
	f.records = append(f.records, *a)
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, nil))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func newTestRetriever(t *testing.T, emb *fakeEmbedder, kn *fakeKnowledge, audit *fakeAudit) *Retriever {
	t.Helper()
	r, err := NewRetriever(
		config.RAGConfig{CosineThreshold: 0.78, TopK: 5, FallbackOnMiss: true},
		emb, kn, audit, testLogger(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// --- retriever ---

func TestRetrieve_HitAuditsAndReturnsChunks(t *testing.T) {
	emb := &fakeEmbedder{vec: []float32{1, 0}}
	kn := &fakeKnowledge{results: []repo.ScoredChunk{
		{Source: "faq.md", Content: "про тарифы", Similarity: 0.91},
		{Source: "faq.md", Content: "про вывод", Similarity: 0.8},
	}}
	audit := &fakeAudit{}
	r := newTestRetriever(t, emb, kn, audit)

	got, err := r.Retrieve(context.Background(), 7, "какие тарифы?")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("получили %d чанков", len(got))
	}
	// Порог и top_k пришли из конфига (критерий приёмки M4).
	if kn.gotTopK != 5 || kn.gotMinSim != 0.78 {
		t.Errorf("topK=%d minSim=%v, ожидали из конфига 5/0.78", kn.gotTopK, kn.gotMinSim)
	}
	// Запрос эмбеддится как query, не document.
	if emb.gotInputType != embeddings.InputQuery {
		t.Errorf("input_type = %q", emb.gotInputType)
	}
	if len(audit.records) != 1 || audit.records[0].Results != 2 ||
		audit.records[0].Threshold != 0.78 || *audit.records[0].LeadID != 7 {
		t.Errorf("rag_audit: %+v", audit.records)
	}
}

func TestRetrieve_MissWritesRagMissAudit(t *testing.T) {
	// AQ²-11 доп.: 0 результатов → не ошибка, rag_audit с results=0.
	emb := &fakeEmbedder{vec: []float32{1, 0}}
	kn := &fakeKnowledge{results: nil}
	audit := &fakeAudit{}
	r := newTestRetriever(t, emb, kn, audit)

	got, err := r.Retrieve(context.Background(), 7, "вопрос не из базы")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ожидали rag_miss, получили %+v", got)
	}
	if len(audit.records) != 1 || audit.records[0].Results != 0 {
		t.Fatalf("rag_miss обязан оставить запись results=0: %+v", audit.records)
	}
	if audit.records[0].QueryText != "вопрос не из базы" {
		t.Errorf("query_text = %q", audit.records[0].QueryText)
	}
}

func TestRetrieve_EmbedderErrorPropagates(t *testing.T) {
	emb := &fakeEmbedder{err: errors.New("voyage 500")}
	audit := &fakeAudit{}
	r := newTestRetriever(t, emb, &fakeKnowledge{}, audit)

	_, err := r.Retrieve(context.Background(), 7, "вопрос")
	if err == nil || !strings.Contains(err.Error(), "voyage 500") {
		t.Fatalf("ошибка Voyage обязана всплыть (ретрай Asynq), получили %v", err)
	}
	if len(audit.records) != 0 {
		t.Error("при ошибке эмбеддинга аудит писать не о чем")
	}
}

func TestRetrieve_AuditFailureDoesNotBreakDialog(t *testing.T) {
	emb := &fakeEmbedder{vec: []float32{1, 0}}
	kn := &fakeKnowledge{results: []repo.ScoredChunk{{Content: "чанк", Similarity: 0.9}}}
	audit := &fakeAudit{err: errors.New("pg down")}
	r := newTestRetriever(t, emb, kn, audit)

	got, err := r.Retrieve(context.Background(), 7, "вопрос")
	if err != nil {
		t.Fatalf("падение аудита не должно ронять диалог: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("чанки должны вернуться несмотря на аудит: %+v", got)
	}
}

// --- indexer ---

func TestIndexDocument_ChunksEmbedsAndReplaces(t *testing.T) {
	emb := &fakeEmbedder{vec: []float32{0.5, 0.5}}
	kn := &fakeKnowledge{}
	ix := NewIndexer(emb, kn, testLogger())

	n, err := ix.IndexDocument(context.Background(), "faq.md",
		"Первый абзац про тарифы.\n\nВторой абзац про вывод средств.")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("короткий документ = 1 чанк, получили %d", n)
	}
	if emb.gotInputType != embeddings.InputDocument {
		t.Errorf("индексация обязана эмбеддить как document, получили %q", emb.gotInputType)
	}
	chunks := kn.replaced["faq.md"]
	if len(chunks) != 1 || len(chunks[0].Embedding) != 2 {
		t.Fatalf("ReplaceSource получил %+v", chunks)
	}
}

func TestIndexDocument_EmptyDocClearsSource(t *testing.T) {
	emb := &fakeEmbedder{}
	kn := &fakeKnowledge{}
	ix := NewIndexer(emb, kn, testLogger())

	n, err := ix.IndexDocument(context.Background(), "old.md", "  \n\n  ")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("пустой документ: %d чанков", n)
	}
	if got, ok := kn.replaced["old.md"]; !ok || len(got) != 0 {
		t.Fatalf("пустой документ обязан вычистить source: %+v", kn.replaced)
	}
	if emb.gotTexts != nil {
		t.Error("нечего эмбеддить — Voyage звать нельзя")
	}
}
