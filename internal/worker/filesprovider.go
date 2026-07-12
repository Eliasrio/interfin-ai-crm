// filesprovider.go — источник списка активных файлов для отправки (EP-04).
//
// Список попадает в system-блок на каждый inbound, поэтому поверх
// репозитория — кэш 30 секунд (паттерн CachedPromptProvider/EP-02):
// правка владельца во вкладке 3 доезжает до Эммы максимум за cacheTTL
// без рестарта процесса (ТЗ §3). Валидация маркеров в кэш НЕ ходит —
// она читает GetByID напрямую, выключенный файл отсекается сразу.
package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// sendFilesCacheTTL — свежесть кэша списка файлов: та же дисциплина 30 с,
// что у settings и промпта («изменения доезжают ≤30 с»).
const sendFilesCacheTTL = 30 * time.Second

// SendFilesProvider — контракт процессора на список активных файлов
// (в тестах — фейк).
type SendFilesProvider interface {
	// Active — активные файлы для секции system-блока. Боевая реализация
	// ошибок наружу не отдаёт (деградация внутри) — файлы не роняют диалог.
	Active(ctx context.Context) ([]models.EmmaSendFile, error)
}

// CachedSendFilesProvider — боевой SendFilesProvider поверх EmmaSendFilesRepo.
type CachedSendFilesProvider struct {
	repo repo.EmmaSendFilesRepo
	log  *slog.Logger

	mu       sync.Mutex
	cached   []models.EmmaSendFile
	hasCache bool
	expires  time.Time

	now func() time.Time // подменяется в тестах кэша
}

func NewSendFilesProvider(r repo.EmmaSendFilesRepo, log *slog.Logger) *CachedSendFilesProvider {
	return &CachedSendFilesProvider{repo: r, log: log, now: time.Now}
}

// Active реализует SendFilesProvider. Ошибка БД деградирует до последнего
// удачно прочитанного списка (протухший кэш лучше внезапно «исчезнувших»
// файлов), без него — до пустого списка (секция не собирается); ошибка НЕ
// кэшируется — следующее чтение попробует снова (дисциплина EP-02).
func (p *CachedSendFilesProvider) Active(ctx context.Context) ([]models.EmmaSendFile, error) {
	p.mu.Lock()
	if p.hasCache && p.now().Before(p.expires) {
		files := p.cached
		p.mu.Unlock()
		return files, nil
	}
	stale, hasStale := p.cached, p.hasCache
	p.mu.Unlock()

	files, err := p.repo.ListActive(ctx)
	if err != nil {
		p.log.Warn("worker: список файлов для отправки не прочитан, работаем на предыдущем",
			"error", err)
		if hasStale {
			return stale, nil
		}
		return nil, nil
	}
	p.mu.Lock()
	p.cached, p.hasCache = files, true
	p.expires = p.now().Add(sendFilesCacheTTL)
	p.mu.Unlock()
	return files, nil
}
