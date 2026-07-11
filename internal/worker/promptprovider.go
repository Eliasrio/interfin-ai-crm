// promptprovider.go — источник системного промпта Эммы для воркера (EP-02).
//
// Промпт редактируется из панели (таблица emma_prompt_versions) и читается
// горячим путём — на каждый inbound. Поверх репозитория — кэш 30 секунд
// (паттерн internal/settings): правка владельца доезжает до Эммы максимум
// за cacheTTL без рестарта процесса (ТЗ EMMA_PANEL_TZ_v2 §3).
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// promptCacheTTL — свежесть кэша промпта: та же дисциплина 30 с, что у
// internal/settings (общий UI-паттерн панели «изменения доезжают ≤30 с»).
const promptCacheTTL = 30 * time.Second

// PromptConfig — активная конфигурация промпта: редактируемые поля вкладки 1
// (ТЗ §3). Контракт наружу: EP-04/EP-05 добавляют свои секции рядом,
// в той же точке сборки buildSystemBase (prompt.go).
type PromptConfig struct {
	Text            string   // системный промпт (база system-блока)
	ForbiddenTopics []string // запретные темы; пусто — секция не добавляется
	Style           string   // formal/friendly/neutral/expert; neutral — без вставки
}

// PromptProvider — контракт процессора на активный промпт (в тестах — фейк).
type PromptProvider interface {
	// Current — актуальная конфигурация. Боевая реализация ошибок наружу
	// не отдаёт (fallback внутри) — промпт не роняет диалог.
	Current(ctx context.Context) (PromptConfig, error)
}

// fallbackPromptConfig — конфигурация из констант кода: пустая таблица
// (свежая БД без сида 0021) или недоступная БД. Поведение Эммы — как до EP-02.
func fallbackPromptConfig() PromptConfig {
	return PromptConfig{Text: systemPrompt, Style: models.EmmaStyleNeutral}
}

// CachedPromptProvider — боевой PromptProvider поверх EmmaPromptRepo.
type CachedPromptProvider struct {
	repo repo.EmmaPromptRepo
	log  *slog.Logger

	mu       sync.Mutex
	cached   PromptConfig
	hasCache bool
	expires  time.Time

	now func() time.Time // подменяется в тестах кэша
}

func NewPromptProvider(r repo.EmmaPromptRepo, log *slog.Logger) *CachedPromptProvider {
	return &CachedPromptProvider{repo: r, log: log, now: time.Now}
}

// Current реализует PromptProvider. Ошибка БД деградирует до последнего
// удачно прочитанного значения (протухший кэш лучше отката поведения Эммы
// на константу), без него — до fallback; ошибка НЕ кэшируется — следующее
// чтение попробует снова (дисциплина settings.String).
func (p *CachedPromptProvider) Current(ctx context.Context) (PromptConfig, error) {
	p.mu.Lock()
	if p.hasCache && p.now().Before(p.expires) {
		cfg := p.cached
		p.mu.Unlock()
		return cfg, nil
	}
	stale, hasStale := p.cached, p.hasCache
	p.mu.Unlock()

	v, err := p.repo.GetCurrent(ctx)
	switch {
	case err == nil:
		cfg := promptConfigFrom(v, p.log)
		p.store(cfg)
		return cfg, nil
	case errors.Is(err, repo.ErrNotFound):
		// Таблица пуста — легальное состояние (свежая БД без 0021):
		// константа кода, кэшируем как обычное значение.
		cfg := fallbackPromptConfig()
		p.store(cfg)
		return cfg, nil
	default:
		p.log.Warn("worker: активный промпт не прочитан, работаем на предыдущем", "error", err)
		if hasStale {
			return stale, nil
		}
		return fallbackPromptConfig(), nil
	}
}

func (p *CachedPromptProvider) store(cfg PromptConfig) {
	p.mu.Lock()
	p.cached, p.hasCache = cfg, true
	p.expires = p.now().Add(promptCacheTTL)
	p.mu.Unlock()
}

// promptConfigFrom — модель БД → конфигурация. Битый JSONB тем (ручная
// правка в БД) деградирует до пустого списка, а не роняет диалог.
func promptConfigFrom(v *models.EmmaPromptVersion, log *slog.Logger) PromptConfig {
	var topics []string
	if len(v.ForbiddenTopics) > 0 {
		if err := json.Unmarshal(v.ForbiddenTopics, &topics); err != nil {
			log.Warn("worker: forbidden_topics не разобраны, секция пропущена",
				"version_id", v.ID, "error", err)
			topics = nil
		}
	}
	return PromptConfig{Text: v.SystemPrompt, ForbiddenTopics: topics, Style: v.Style}
}
