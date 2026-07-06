// Package embeddings — HTTP-клиент Voyage AI Embeddings API (SRS §7.1).
//
// AQ²-fix #2 / CLAUDE.md §3: эмбеддинги — ТОЛЬКО Voyage voyage-3 (1024 dims),
// никакого OpenAI и OPENAI_API_KEY. Официальный SDK не используется по той же
// причине, что и для Claude (M3): зависимости пришпилены под Go 1.22,
// а нужен единственный вызов POST /v1/embeddings.
package embeddings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/config"
)

const (
	defaultBaseURL = "https://api.voyageai.com"
	// httpTimeout — потолок ожидания Voyage. Эмбеддинг — лёгкий вызов
	// (сотни мс), 30 с хватает и на батч индексации.
	httpTimeout = 30 * time.Second
	// maxBatch — лимит Voyage API на число текстов в одном запросе.
	maxBatch = 128
)

// Типы входа Voyage: разметка «что эмбеддится» повышает качество retrieval —
// запросы и документы проецируются согласованно.
const (
	InputQuery    = "query"    // запрос лида (retrieval)
	InputDocument = "document" // чанк базы знаний (индексация)
)

// Client — потокобезопасный клиент; создаётся один раз на процесс.
type Client struct {
	apiKey     string
	model      string
	dimensions int
	baseURL    string
	httpClient *http.Client
}

// New собирает клиента из embeddings-секции конфига (§12).
func New(cfg config.EmbeddingsConfig) (*Client, error) {
	if cfg.Provider != "voyage" {
		return nil, fmt.Errorf("embeddings: провайдер %q не поддержан, только voyage (AQ²-2)", cfg.Provider)
	}
	if cfg.APIKey == "" {
		return nil, errors.New("embeddings: пустой api_key (VOYAGE_API_KEY)")
	}
	if cfg.Model == "" {
		return nil, errors.New("embeddings: пустой model")
	}
	if cfg.Dimensions <= 0 {
		return nil, fmt.Errorf("embeddings: dimensions = %d, ожидали > 0", cfg.Dimensions)
	}
	return &Client{
		apiKey:     cfg.APIKey,
		model:      cfg.Model,
		dimensions: cfg.Dimensions,
		baseURL:    defaultBaseURL,
		httpClient: &http.Client{Timeout: httpTimeout},
	}, nil
}

type embedRequest struct {
	Input     []string `json:"input"`
	Model     string   `json:"model"`
	InputType string   `json:"input_type,omitempty"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

// apiError — формат ошибки Voyage: {"detail": "..."}.
type apiError struct {
	Detail string `json:"detail"`
}

// Embed возвращает эмбеддинг на каждый входной текст (порядок сохраняется).
// inputType — InputQuery|InputDocument. Батчи больше лимита API режутся
// прозрачно для вызывающего. Размерность каждого вектора проверяется:
// не-1024 молча не запишется в vector(1024) (§7.1) — лучше явная ошибка тут.
func (c *Client) Embed(ctx context.Context, texts []string, inputType string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, errors.New("embeddings: пустой список текстов")
	}
	if inputType != InputQuery && inputType != InputDocument {
		return nil, fmt.Errorf("embeddings: неизвестный input_type %q", inputType)
	}

	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += maxBatch {
		end := start + maxBatch
		if end > len(texts) {
			end = len(texts)
		}
		batch, err := c.embedBatch(ctx, texts[start:end], inputType)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
	}
	return out, nil
}

func (c *Client) embedBatch(ctx context.Context, texts []string, inputType string) ([][]float32, error) {
	payload, err := json.Marshal(embedRequest{Input: texts, Model: c.model, InputType: inputType})
	if err != nil {
		return nil, fmt.Errorf("embeddings: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("embeddings: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings: do request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("embeddings: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var ae apiError
		if json.Unmarshal(raw, &ae) == nil && ae.Detail != "" {
			return nil, fmt.Errorf("embeddings: api %s: %s", resp.Status, ae.Detail)
		}
		return nil, fmt.Errorf("embeddings: api %s", resp.Status)
	}

	var er embedResponse
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, fmt.Errorf("embeddings: unmarshal response: %w", err)
	}
	if len(er.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: ожидали %d векторов, получили %d", len(texts), len(er.Data))
	}

	// API документирует порядок по index — раскладываем по нему явно.
	vecs := make([][]float32, len(texts))
	for _, d := range er.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return nil, fmt.Errorf("embeddings: index %d вне диапазона ответа", d.Index)
		}
		if len(d.Embedding) != c.dimensions {
			return nil, fmt.Errorf("embeddings: вектор размерности %d, ожидали %d (§7.1)",
				len(d.Embedding), c.dimensions)
		}
		vecs[d.Index] = d.Embedding
	}
	for i, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("embeddings: в ответе нет вектора для текста %d", i)
		}
	}
	return vecs, nil
}
