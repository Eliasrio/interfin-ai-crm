package embeddings

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/interfin/interfin-ai-crm/internal/config"
)

func testConfig() config.EmbeddingsConfig {
	return config.EmbeddingsConfig{
		Provider:   "voyage",
		APIKey:     "test-key",
		Model:      "voyage-3",
		Dimensions: 4, // компактно для тестов; в проде 1024 (§7.1)
	}
}

// newTestClient — клиент, направленный в httptest-сервер.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.baseURL = srv.URL
	return c
}

// vec — детерминированный вектор нужной размерности для текста i.
func vec(i int) []float32 {
	return []float32{float32(i), float32(i), float32(i), float32(i)}
}

func respondEmbeddings(w http.ResponseWriter, n int) {
	type item struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	}
	items := make([]item, n)
	for i := range items {
		items[i] = item{Embedding: vec(i), Index: i}
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": items})
}

func TestEmbed_RequestShape(t *testing.T) {
	var got embedRequest
	var auth string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %q, ожидали /v1/embeddings", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		respondEmbeddings(w, len(got.Input))
	})

	vecs, err := c.Embed(context.Background(), []string{"вопрос лида"}, InputQuery)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if auth != "Bearer test-key" {
		t.Errorf("Authorization = %q", auth)
	}
	if got.Model != "voyage-3" || got.InputType != "query" || len(got.Input) != 1 {
		t.Errorf("request = %+v", got)
	}
	if len(vecs) != 1 || len(vecs[0]) != 4 {
		t.Fatalf("vecs = %v", vecs)
	}
}

func TestEmbed_BatchesOverAPILimit(t *testing.T) {
	var batchSizes []int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		batchSizes = append(batchSizes, len(req.Input))
		respondEmbeddings(w, len(req.Input))
	})

	texts := make([]string, maxBatch+3)
	for i := range texts {
		texts[i] = fmt.Sprintf("чанк %d", i)
	}
	vecs, err := c.Embed(context.Background(), texts, InputDocument)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("получили %d векторов, ожидали %d", len(vecs), len(texts))
	}
	if len(batchSizes) != 2 || batchSizes[0] != maxBatch || batchSizes[1] != 3 {
		t.Errorf("batchSizes = %v, ожидали [%d 3]", batchSizes, maxBatch)
	}
}

func TestEmbed_ResponseOrderedByIndex(t *testing.T) {
	// Ответ с перепутанным порядком data — векторы обязаны лечь по index.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[
			{"embedding":[1,1,1,1],"index":1},
			{"embedding":[0,0,0,0],"index":0}]}`)
	})
	vecs, err := c.Embed(context.Background(), []string{"a", "b"}, InputQuery)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if vecs[0][0] != 0 || vecs[1][0] != 1 {
		t.Errorf("порядок нарушен: %v", vecs)
	}
}

func TestEmbed_WrongDimensionsRejected(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"embedding":[1,2],"index":0}]}`)
	})
	_, err := c.Embed(context.Background(), []string{"x"}, InputQuery)
	if err == nil || !strings.Contains(err.Error(), "размерности") {
		t.Fatalf("ожидали ошибку размерности, получили %v", err)
	}
}

func TestEmbed_APIErrorSurfaced(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"detail":"Provided API key is invalid."}`)
	})
	_, err := c.Embed(context.Background(), []string{"x"}, InputQuery)
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("ожидали ошибку API, получили %v", err)
	}
}

func TestNew_RejectsNonVoyage(t *testing.T) {
	cfg := testConfig()
	cfg.Provider = "openai"
	if _, err := New(cfg); err == nil {
		t.Fatal("провайдер openai обязан отвергаться (AQ²-2)")
	}
}

func TestEmbed_RejectsUnknownInputType(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("запрос не должен был уйти в API")
	})
	if _, err := c.Embed(context.Background(), []string{"x"}, "banana"); err == nil {
		t.Fatal("ожидали ошибку input_type")
	}
}
