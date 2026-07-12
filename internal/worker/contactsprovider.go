// contactsprovider.go — источник списка активных контактов для секции
// system-блока (EP-05, ТЗ §3 вкладка 4).
//
// Список попадает в system-блок на каждый inbound, поэтому поверх
// репозитория — кэш 30 секунд (паттерн CachedSendFilesProvider/EP-04):
// правка справочника во вкладке 4 доезжает до Эммы максимум за cacheTTL
// без рестарта процесса.
package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// contactsCacheTTL — свежесть кэша списка контактов: та же дисциплина 30 с,
// что у settings, промпта и файлов («изменения доезжают ≤30 с»).
const contactsCacheTTL = 30 * time.Second

// ContactsProvider — контракт процессора на список активных контактов
// (в тестах — фейк).
type ContactsProvider interface {
	// Active — активные контакты для секции system-блока (sort_order ASC).
	// Боевая реализация ошибок наружу не отдаёт (деградация внутри) —
	// контакты не роняют диалог.
	Active(ctx context.Context) ([]models.EmmaContact, error)
}

// CachedContactsProvider — боевой ContactsProvider поверх EmmaContactsRepo.
type CachedContactsProvider struct {
	repo repo.EmmaContactsRepo
	log  *slog.Logger

	mu       sync.Mutex
	cached   []models.EmmaContact
	hasCache bool
	expires  time.Time

	now func() time.Time // подменяется в тестах кэша
}

func NewContactsProvider(r repo.EmmaContactsRepo, log *slog.Logger) *CachedContactsProvider {
	return &CachedContactsProvider{repo: r, log: log, now: time.Now}
}

// Active реализует ContactsProvider. Ошибка БД деградирует до последнего
// удачно прочитанного списка, без него — до пустого (секции нет); ошибка
// НЕ кэшируется — следующее чтение попробует снова (дисциплина EP-04).
func (p *CachedContactsProvider) Active(ctx context.Context) ([]models.EmmaContact, error) {
	p.mu.Lock()
	if p.hasCache && p.now().Before(p.expires) {
		contacts := p.cached
		p.mu.Unlock()
		return contacts, nil
	}
	stale, hasStale := p.cached, p.hasCache
	p.mu.Unlock()

	contacts, err := p.repo.ListActive(ctx)
	if err != nil {
		p.log.Warn("worker: список контактов для секции не прочитан, работаем на предыдущем",
			"error", err)
		if hasStale {
			return stale, nil
		}
		return nil, nil
	}
	p.mu.Lock()
	p.cached, p.hasCache = contacts, true
	p.expires = p.now().Add(contactsCacheTTL)
	p.mu.Unlock()
	return contacts, nil
}
