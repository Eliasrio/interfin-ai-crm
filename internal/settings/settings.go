// Package settings — настройки CRM, редактируемые из UI (M13, таблица 0014).
//
// Таблица settings хранит только переопределения; дефолты живут здесь.
// Значения читаются горячим путём (processor на каждый inbound), поэтому
// поверх репозитория — кэш на 30 секунд: смена настройки доезжает до
// воркера максимум за cacheTTL, БД не дёргается на каждое сообщение.
//
// В M13 все ключи — интервалы контура takeover в минутах (1..1440 через
// API; сервис читает и 0 — прямой сид в БД для тестов «пауза уже истекла»).
package settings

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// Ключи M13 (takeover). Новые ключи блока 3 (Command Center, M15)
// добавляются сюда же вместе с дефолтом.
const (
	// KeyHybridPauseMinutes — пауза Эммы после реплики менеджера из карточки
	// при dialog_mode='bot' (автопилот hybrid).
	KeyHybridPauseMinutes = "takeover.hybrid_pause_minutes"
	// KeyReminderMinutes — через сколько минут тишины в режиме human/паузы
	// менеджеру уходит напоминание об ожидающем клиенте.
	KeyReminderMinutes = "takeover.reminder_minutes"
	// KeyPickupMinutes — через сколько минут после напоминания Эмма
	// подхватывает диалог сама, если менеджер так и не ответил.
	KeyPickupMinutes = "takeover.pickup_minutes"
)

// Defaults — значение ключа при отсутствии строки в settings (тайминги
// владельца: пауза 30, напоминание 10, подхват 10 — task M13).
var Defaults = map[string]int{
	KeyHybridPauseMinutes: 30,
	KeyReminderMinutes:    10,
	KeyPickupMinutes:      10,
}

// Строковые ключи панели Эммы (EP-01, ТЗ EMMA_PANEL_TZ_v2 §5).
const (
	// KeyPinHash — bcrypt-хэш PIN панели. СЛУЖЕБНЫЙ: не отдаётся через
	// GET /api/settings и не принимается через PATCH — только internal/emma.
	KeyPinHash = "emma_panel.pin_hash"
	// KeyPromptTokenLimit — лимит токенов системного промпта (EP-02).
	KeyPromptTokenLimit = "emma_panel.prompt_token_limit"
	// KeyWelcomeText — приветствие /start (EP-05).
	KeyWelcomeText = "emma_panel.welcome_text"
	// KeyManagerButtonEnabled — показывать ли кнопку «Связаться с менеджером» (EP-05).
	KeyManagerButtonEnabled = "emma_panel.manager_button_enabled"
	// KeyManagerButtonText — подпись кнопки менеджера (EP-05).
	KeyManagerButtonText = "emma_panel.manager_button_text"
	// KeyHandoffConfirmText — ответ клиенту при переводе на менеджера (EP-05).
	KeyHandoffConfirmText = "emma_panel.handoff_confirm_text"
	// KeyAlertChatID — Telegram-чат владельца для алертов Эммы (EP-06).
	KeyAlertChatID = "emma_panel.alert_chat_id"
)

// StringDefaults — дефолты строковых ключей (EP-01 §2 task-файла).
// Типизация значения (bool/int) — на потребителе, здесь только строки.
var StringDefaults = map[string]string{
	KeyPinHash:              "",
	KeyPromptTokenLimit:     "1200",
	KeyWelcomeText:          "",
	KeyManagerButtonEnabled: "false",
	KeyManagerButtonText:    "Связаться с менеджером",
	KeyHandoffConfirmText:   "Сейчас свяжу вас с менеджером, ожидайте",
	KeyAlertChatID:          "",
}

// Границы значений для PATCH /api/settings: минуты 1..1440 (сутки).
const (
	MinMinutes = 1
	MaxMinutes = 1440
)

// cacheTTL — свежесть кэша чтения. 30 секунд — задача M13 §2.
const cacheTTL = 30 * time.Second

// Ошибки валидации SetMinutes — хендлер мапит их в 400 ERR_VALIDATION.
var (
	ErrUnknownKey = errors.New("settings: неизвестный ключ")
	ErrOutOfRange = fmt.Errorf("settings: значение вне %d..%d минут", MinMinutes, MaxMinutes)
)

// Reader — читающий контракт сервиса для processor/handlers (в тестах — фейк).
type Reader interface {
	// Minutes — значение ключа в минутах: переопределение из БД или дефолт.
	// Ошибки чтения деградируют до дефолта — настройки не роняют диалог.
	Minutes(ctx context.Context, key string) int
}

// Service — типизированный доступ к settings с кэшем.
type Service struct {
	repo repo.SettingsRepo

	mu       sync.Mutex
	cache    map[string]cacheEntry
	strCache map[string]strCacheEntry // строковые ключи EP-01 — отдельный кэш, та же дисциплина

	now func() time.Time // подменяется в тестах кэша
}

type cacheEntry struct {
	val     int
	expires time.Time
}

type strCacheEntry struct {
	val     string
	expires time.Time
}

func New(r repo.SettingsRepo) *Service {
	return &Service{
		repo:     r,
		cache:    map[string]cacheEntry{},
		strCache: map[string]strCacheEntry{},
		now:      time.Now,
	}
}

// Minutes реализует Reader. Нечисловое значение в БД (легаси/ручная правка)
// приравнивается к отсутствию строки — дефолт.
func (s *Service) Minutes(ctx context.Context, key string) int {
	def := Defaults[key]

	s.mu.Lock()
	if e, ok := s.cache[key]; ok && s.now().Before(e.expires) {
		s.mu.Unlock()
		return e.val
	}
	s.mu.Unlock()

	raw, err := s.repo.Get(ctx, key)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		s.store(key, def)
		return def
	case err != nil:
		// БД недоступна — отвечаем дефолтом и НЕ кэшируем: следующее чтение
		// попробует снова (иначе залипли бы на дефолте на весь TTL).
		return def
	}
	val, perr := strconv.Atoi(raw)
	if perr != nil {
		val = def
	}
	s.store(key, val)
	return val
}

// All — эффективные значения всех известных ключей (GET /api/settings).
func (s *Service) All(ctx context.Context) map[string]int {
	out := make(map[string]int, len(Defaults))
	for key := range Defaults {
		out[key] = s.Minutes(ctx, key)
	}
	return out
}

// SetMinutes валидирует и сохраняет переопределение (PATCH /api/settings,
// только admin). Кэш ключа сбрасывается сразу — процесс, принявший PATCH,
// видит новое значение немедленно; остальные реплики — в пределах cacheTTL.
func (s *Service) SetMinutes(ctx context.Context, key string, minutes int) error {
	if _, ok := Defaults[key]; !ok {
		return fmt.Errorf("%w: %s", ErrUnknownKey, key)
	}
	if minutes < MinMinutes || minutes > MaxMinutes {
		return fmt.Errorf("%w: %s=%d", ErrOutOfRange, key, minutes)
	}
	if err := s.repo.Set(ctx, key, strconv.Itoa(minutes)); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.cache, key)
	s.mu.Unlock()
	return nil
}

func (s *Service) store(key string, val int) {
	s.mu.Lock()
	s.cache[key] = cacheEntry{val: val, expires: s.now().Add(cacheTTL)}
	s.mu.Unlock()
}

// --- Строковые ключи (EP-01): та же семантика, что у Minutes/SetMinutes ---

// String — значение строкового ключа: переопределение из БД или дефолт из
// StringDefaults. Ошибки чтения деградируют до дефолта и НЕ кэшируются
// (как Minutes). Для неизвестного ключа — "" (потребители ходят по константам).
func (s *Service) String(ctx context.Context, key string) string {
	def := StringDefaults[key]

	s.mu.Lock()
	if e, ok := s.strCache[key]; ok && s.now().Before(e.expires) {
		s.mu.Unlock()
		return e.val
	}
	s.mu.Unlock()

	raw, err := s.repo.Get(ctx, key)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		s.storeString(key, def)
		return def
	case err != nil:
		// БД недоступна — дефолт без кэширования: следующее чтение попробует
		// снова (та же дисциплина, что у Minutes).
		return def
	}
	s.storeString(key, raw)
	return raw
}

// SetString валидирует ключ и сохраняет переопределение. Кэш ключа
// сбрасывается сразу — процесс, принявший запись, видит новое значение
// немедленно; остальные реплики — в пределах cacheTTL.
func (s *Service) SetString(ctx context.Context, key, val string) error {
	if _, ok := StringDefaults[key]; !ok {
		return fmt.Errorf("%w: %s", ErrUnknownKey, key)
	}
	if err := s.repo.Set(ctx, key, val); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.strCache, key)
	s.mu.Unlock()
	return nil
}

func (s *Service) storeString(key, val string) {
	s.mu.Lock()
	s.strCache[key] = strCacheEntry{val: val, expires: s.now().Add(cacheTTL)}
	s.mu.Unlock()
}
