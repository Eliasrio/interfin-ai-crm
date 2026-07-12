// Package repo — repository-слой над GORM (контракт M1 наружу).
//
// Интерфейсы LeadRepo / MessageRepo / PaymentRepo — единственная точка доступа
// бизнес-логики (M2+) к таблицам §8. Прямые запросы GORM вне этого пакета
// запрещены: инварианты вроде «message_count считает только inbound»
// (CLAUDE.md §4.3) живут именно здесь.
package repo

import (
	"context"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

// ErrNotFound возвращают Get*-методы, когда записи нет.
// Обёртка над gorm.ErrRecordNotFound, чтобы вызывающие не зависели от GORM.
var ErrNotFound = errNotFound{}

type errNotFound struct{}

func (errNotFound) Error() string { return "repo: запись не найдена" }

// LeadRepo — leads (§8.1).
type LeadRepo interface {
	// Create вставляет нового лида; заполняет lead.ID.
	Create(ctx context.Context, lead *models.Lead) error
	// GetByID возвращает лида по PK. ErrNotFound, если нет или стёрт (deleted_at).
	GetByID(ctx context.Context, id int64) (*models.Lead, error)
	// GetByTelegramUserID — основная выборка ingestion-пайплайна (M2).
	GetByTelegramUserID(ctx context.Context, tgUserID int64) (*models.Lead, error)
	// Save перезаписывает все поля лида (lead.ID обязан быть заполнен).
	Save(ctx context.Context, lead *models.Lead) error
	// UpdateFields точечно меняет колонки по PK, например
	// {"pending_task": true} при Redis down (§11.2).
	UpdateFields(ctx context.Context, id int64, fields map[string]interface{}) error
	// List — постраничный список живых (deleted_at IS NULL) лидов для
	// GET /api/leads (M8, §4.1), свежая активность первой. UpdatedSince —
	// catch-up §10.3: только лиды с last_activity_at позже метки (быстро:
	// idx_leads_activity). Возвращает страницу и total под пагинацию M10.
	List(ctx context.Context, p ListLeadsParams) ([]models.Lead, int64, error)
	// ListPendingTask — лиды с pending_task=TRUE (Redis был недоступен на
	// enqueue, §11.2); выборка ложится на частичный индекс idx_leads_pending.
	// Для recovery-cron M11: старые первыми (id ASC), не больше limit за тик.
	ListPendingTask(ctx context.Context, limit int) ([]models.Lead, error)
	// ClearPendingTask снимает pending_task, ТОЛЬКО если message_count не
	// изменился с момента чтения лида (seenMessageCount). false — лид успел
	// получить новое сообщение: флаг не трогаем, следующий тик recovery-cron
	// перевыставит задачу уже с новым счётчиком (§11.2, гонка recovery/webhook).
	ClearPendingTask(ctx context.Context, id int64, seenMessageCount int) (bool, error)
	// TransitionStage атомарно (CAS: WHERE stage_id = from) переводит лида
	// в стадию to, сбрасывает anti_spam_count (§3.5) и обновляет
	// last_activity_at — точку отсчёта TTL новой стадии (CLAUDE.md §4.7).
	// false — лид уже не в from: конкурирующий переход победил
	// (Manual/Payment переопределяют авто-триггеры, §3.1) — вызывающий
	// обязан НЕ применять свой переход.
	TransitionStage(ctx context.Context, id int64, from, to int16) (bool, error)
}

// MessageRepo — messages (§8.2).
type MessageRepo interface {
	// CreateInbound атомарно (одна транзакция): вставляет сообщение
	// direction='inbound' и у лида message_count+1, anti_spam_count+1,
	// last_activity_at=NOW(). Только inbound двигает счётчики (CLAUDE.md §4.3).
	CreateInbound(ctx context.Context, msg *models.Message) error
	// CreateInboundSetLanguage — как CreateInbound, плюс ТОЙ ЖЕ транзакцией
	// выставляет leads.language = language, если он ещё NULL (M14: детекция
	// первого текстового inbound). Гонку двух первых сообщений решает guard
	// WHERE language IS NULL: язык записывает ровно одна транзакция —
	// langSet=true, вызывающий публикует WS-событие lead_language.
	CreateInboundSetLanguage(ctx context.Context, msg *models.Message, language string) (langSet bool, err error)
	// CreateOutbound вставляет ответ бота/менеджера. Счётчики НЕ трогает.
	CreateOutbound(ctx context.Context, msg *models.Message) error
	// ListByLead — последние limit сообщений лида, от старых к новым
	// (порядок, в котором история уходит в контекст Claude).
	ListByLead(ctx context.Context, leadID int64, limit int) ([]models.Message, error)
	// ListByLeadBefore — страница истории для чата M12: последние limit
	// сообщений с id < beforeID (0 — просто последние), от старых к новым.
	// Прокрутка вверх: клиент передаёт id старейшего загруженного сообщения.
	ListByLeadBefore(ctx context.Context, leadID, beforeID int64, limit int) ([]models.Message, error)
	// HasManagerOutboundAfter — ответил ли менеджер лиду ПОСЛЕ сообщения
	// afterID: есть outbound с author 'manager:%' и id > afterID (M13,
	// no-op-проверка напоминаний/подхвата — смотрит на факт ответа, а не на
	// конкретный message_id, поэтому устаревшие задачи гаснут сами).
	HasManagerOutboundAfter(ctx context.Context, leadID, afterID int64) (bool, error)
}

// ListLeadsParams — параметры LeadRepo.List. Limit <= 0 недопустим
// (значение по умолчанию выбирает хендлер, не репозиторий).
type ListLeadsParams struct {
	UpdatedSince *time.Time // catch-up §10.3; nil — без фильтра
	Limit        int
	Offset       int
}

// LGPDRepo — erasure/export/retention (§9, M8). Отдельный интерфейс:
// операции пересекают таблицы leads/messages/lgpd_audit и намеренно
// обходят soft-delete-скоуп GORM (Unscoped) — в LeadRepo им не место.
type LGPDRepo interface {
	// Erase — «право на забвение» §9.1/§9.3 одной транзакцией:
	// deleted_at=NOW(), name/phone/tg_username=NULL,
	// telegram_user_id=hashedTgID (НЕ NULL — колонка UNIQUE NOT NULL),
	// messages.content='[DELETED]', запись в lgpd_audit.
	// payment_events НЕ трогает (CLAUDE.md §4.8). ErrNotFound — лида нет
	// или он уже стёрт (повторный erase не перезатирает хеш).
	Erase(ctx context.Context, leadID int64, hashedTgID int64, performedBy, ip string) error
	// GetLeadAny — лид по PK, ВКЛЮЧАЯ стёртых (export §9.1 обязан работать
	// и после erasure — право на доступ не гаснет вместе с данными).
	GetLeadAny(ctx context.Context, id int64) (*models.Lead, error)
	// CreateAudit — след действия с персональными данными (§9.2).
	CreateAudit(ctx context.Context, a *models.LGPDAudit) error
	// ListAuditByLead — записи lgpd_audit лида, от старых к новым (export).
	ListAuditByLead(ctx context.Context, leadID int64) ([]models.LGPDAudit, error)
	// DeleteErasedBefore — retention-cron §9.1: ФИЗИЧЕСКОЕ удаление лидов,
	// стёртых (deleted_at) раньше cutoff. messages уходят каскадом,
	// payment_events выживают с lead_id=NULL (FK SET NULL, миграция 0010).
	// Возвращает число удалённых лидов.
	DeleteErasedBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// SettingsRepo — settings (M13, 0014): переопределения настроек CRM.
// Дефолты и типизация значений живут выше, в internal/settings.
type SettingsRepo interface {
	// Get возвращает значение ключа. ErrNotFound — переопределения нет
	// (вызывающий подставляет дефолт).
	Get(ctx context.Context, key string) (string, error)
	// Set создаёт или перезаписывает значение ключа (upsert).
	Set(ctx context.Context, key, value string) error
}

// EmmaPromptRepo — emma_prompt_versions (EP-02, ТЗ панели §3/§5).
// Версии промпта не мутируются: любое изменение (в т.ч. restore) — новая
// строка через CreateVersion, старая активная остаётся в истории сама.
type EmmaPromptRepo interface {
	// GetCurrent — активная версия (is_current). ErrNotFound — таблица пуста
	// (свежая БД без сида 0021): вызывающий падает на fallback-константу.
	GetCurrent(ctx context.Context) (*models.EmmaPromptVersion, error)
	// CreateVersion одной транзакцией снимает is_current со старой активной
	// и вставляет v активной (v.IsCurrent выставляется здесь). Гонку двух
	// конкурентных вызовов решает частичный уникальный индекс 0016: проигравший
	// получает ошибку транзакции, а не молчаливую перезапись — вторая активная
	// невозможна. Заполняет v.ID/v.CreatedAt.
	CreateVersion(ctx context.Context, v *models.EmmaPromptVersion) error
	// History — страница истории (новые → старые) и total для пагинации.
	// Превью — первые 100 СИМВОЛОВ промпта (LEFT() Postgres режет по рунам,
	// не байтам — кириллица не рвётся). page с 1, perPage > 0.
	History(ctx context.Context, page, perPage int) ([]EmmaPromptHistoryItem, int64, error)
	// GetByID — полная версия для просмотра/restore. ErrNotFound — нет id.
	GetByID(ctx context.Context, id int64) (*models.EmmaPromptVersion, error)
}

// EmmaPromptHistoryItem — строка боковой панели истории (ТЗ §3): полная
// версия не поднимается, промпт обрезан до превью ещё в SQL.
type EmmaPromptHistoryItem struct {
	ID        int64     `gorm:"column:id"`
	CreatedAt time.Time `gorm:"column:created_at"`
	CreatedBy *int64    `gorm:"column:created_by"` // NULL — сид миграции/удалённый менеджер
	Preview   string    `gorm:"column:preview"`    // первые 100 рун системного промпта
	Style     string    `gorm:"column:style"`
}

// EmmaKBRepo — emma_kb_files (EP-03, ТЗ панели §3): файлы базы знаний панели.
type EmmaKBRepo interface {
	// List — все файлы, новые → старые.
	List(ctx context.Context) ([]models.EmmaKBFile, error)
	// GetByID — файл по id. ErrNotFound, если нет.
	GetByID(ctx context.Context, id int64) (*models.EmmaKBFile, error)
	// UpsertByFilename вставляет строку файла или, при совпадении filename,
	// обновляет существующую (QA-фикс ТЗ §3: id стабилен → source panel:<id>
	// совпадает → ReplaceSource атомарно заменяет старые чанки). Строка
	// сбрасывается в pending: новые mime/path/size, index_error NULL,
	// chunks_count 0. Заполняет f.ID/f.CreatedAt; возвращает file_path
	// заменённой строки ("" — чистая вставка) — вызывающий удаляет
	// вытесненный оригинал с диска.
	UpsertByFilename(ctx context.Context, f *models.EmmaKBFile) (oldPath string, err error)
	// SetStatus пишет итог индексации: status (модельные константы EmmaKB*),
	// chunks_count, index_error (nil — сброс). ErrNotFound — строки уже нет
	// (файл удалили, пока задача стояла в очереди).
	SetStatus(ctx context.Context, id int64, status string, chunks int, errText *string) error
	// Delete одной транзакцией вычищает чанки source файла из
	// knowledge_chunks и удаляет строку (ТЗ §3: [Удалить]). Возвращает
	// удалённую строку — file_path нужен, чтобы стереть оригинал с диска
	// ПОСЛЕ коммита. ErrNotFound — нет id.
	Delete(ctx context.Context, id int64) (*models.EmmaKBFile, error)
}

// EmmaSendFilesRepo — emma_send_files (EP-04, ТЗ панели §3): библиотека
// файлов, которые Эмма отправляет клиентам по маркеру {{file:N}}.
type EmmaSendFilesRepo interface {
	// List — все файлы, новые → старые (вкладка 3).
	List(ctx context.Context) ([]models.EmmaSendFile, error)
	// GetByID — файл по id. ErrNotFound, если нет. Валидация маркеров идёт
	// сюда, мимо 30-секундного кэша секции промпта: выключенный файл
	// перестаёт проходить валидацию сразу (критерий приёмки EP-04).
	GetByID(ctx context.Context, id int64) (*models.EmmaSendFile, error)
	// Create вставляет файл; заполняет f.ID/f.CreatedAt. IsActive задаёт
	// вызывающий явно (грабля M5: zero value затёр бы DEFAULT TRUE).
	Create(ctx context.Context, f *models.EmmaSendFile) error
	// Update точечно меняет name/description/is_active (nil — поле не
	// трогается); возвращает обновлённую строку. ErrNotFound — нет id.
	Update(ctx context.Context, id int64, upd EmmaSendFileUpdate) (*models.EmmaSendFile, error)
	// Delete удаляет строку и возвращает её — file_path нужен, чтобы стереть
	// файл с диска ПОСЛЕ коммита. emma_events.send_file_id обнуляет БД
	// (ON DELETE SET NULL, 0020). ErrNotFound — нет id.
	Delete(ctx context.Context, id int64) (*models.EmmaSendFile, error)
	// ListActive — только is_active, старые → новые (стабильный порядок
	// секции промпта). Источник секции файлов system-блока.
	ListActive(ctx context.Context) ([]models.EmmaSendFile, error)
}

// EmmaSendFileUpdate — частичное обновление файла (PATCH ТЗ §6):
// nil-поле не меняется.
type EmmaSendFileUpdate struct {
	Name        *string
	Description *string
	IsActive    *bool
}

// EmmaEventsRepo — emma_events (EP-04 пишет file_sent/error, EP-06 читает
// для статистики). Запись — best effort вызывающего: журнал не роняет диалог.
type EmmaEventsRepo interface {
	// Create вставляет событие; заполняет ev.ID/ev.CreatedAt.
	Create(ctx context.Context, ev *models.EmmaEvent) error
}

// PaymentRepo — payment_events (§8.3).
type PaymentRepo interface {
	// Create вставляет событие платёжного gateway. Идемпотентен по
	// (gateway, raw_payload->>'update_id') — повтор вебхука не плодит
	// вторую фискальную запись (индекс 0008, M6).
	Create(ctx context.Context, ev *models.PaymentEvent) error
	// ListByLead — все события лида, от старых к новым.
	ListByLead(ctx context.Context, leadID int64) ([]models.PaymentEvent, error)
}
