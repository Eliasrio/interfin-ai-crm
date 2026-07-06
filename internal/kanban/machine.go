// machine.go — динамика state machine (M5): CAS-переход и его side effects.
//
// Каждый успешный переход стадии атомарен в БД (repo.TransitionStage,
// WHERE stage_id=from) и влечёт, в порядке выполнения:
//  1. сброс anti_spam_count (в том же UPDATE, §3.5);
//  2. снятие отложенных antispam:followup / antispam:escalate;
//  3. перевзвод TTL под новую стадию: 4 → 48ч, 6 → 5 дн, остальные — снятие
//     (§3.4; delay считается от «сейчас» = момента активности, CLAUDE.md §4.7);
//  4. PUBLISH crm:events (§10.1; fire-and-forget: ошибка публикации логируется,
//     переход не откатывается — пропуск добирает catch-up M9 §10.3).
//
// Ошибки шагов 2–3 возвращаются наверх (ретрай Asynq/HTTP): повторный вызов
// попадает в ветку «лид уже в to» и идемпотентно доводит side effects.
package kanban

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// ErrInvalidTransition — переход запрещён таблицей §3.1 (M8 → HTTP 400).
var ErrInvalidTransition = errors.New("kanban: переход запрещён таблицей стадий §3.1")

// ErrStageConflict — CAS промахнулся: конкурирующий переход успел раньше
// (M8 → HTTP 409 state conflict). Для авто-триггеров это штатный исход:
// Manual/Payment переопределяют их, а не наоборот (§3.1).
var ErrStageConflict = errors.New("kanban: стадия лида изменена конкурентно")

// followupText — единственное follow-up-сообщение после 24ч anti-spam
// молчания (§3.5, AQ²-fix #8).
const followupText = "Спасибо за ваши сообщения! Передали диалог менеджеру — " +
	"он подключится и ответит вам в ближайшее время."

// TTLScheduler — управление ttl:expire (боевая реализация — queue.TTLManager, M3).
type TTLScheduler interface {
	Schedule(ctx context.Context, leadID int64, delay time.Duration) error
	Cancel(ctx context.Context, leadID int64) error
}

// Sender — отправка сообщений лиду в Telegram (worker.TelebotSender подходит
// структурно; в тестах — фейк).
type Sender interface {
	Send(chatID int64, text string) error
}

// Machine — state machine Kanban. Контракт наружу:
//   - Transition с ActorManager — ручные переходы для M8 (PATCH stage);
//   - Transition с ActorPayment — для M6 (payment webhook);
//   - OnInbound / Handle* — для воркера (M5).
type Machine struct {
	leads    repo.LeadRepo
	msgs     repo.MessageRepo
	ttl      TTLScheduler
	antiSpam queue.AntiSpamScheduler
	pub      events.Publisher
	sender   Sender
	cfg      config.KanbanConfig
	log      *slog.Logger
}

func NewMachine(
	leads repo.LeadRepo,
	msgs repo.MessageRepo,
	ttl TTLScheduler,
	antiSpam queue.AntiSpamScheduler,
	pub events.Publisher,
	sender Sender,
	cfg config.KanbanConfig,
	log *slog.Logger,
) *Machine {
	return &Machine{
		leads: leads, msgs: msgs, ttl: ttl, antiSpam: antiSpam,
		pub: pub, sender: sender, cfg: cfg, log: log,
	}
}

// Transition переводит лида в стадию to от имени actor. Возвращает лида
// в состоянии после перехода.
//
// Идемпотентность: если лид уже в to, переход считается выполненным —
// side effects доводятся повторно (они идемпотентны), ошибки не возникает.
// Это закрывает ретраи «CAS прошёл, side effect упал» и дубли вебхуков.
func (m *Machine) Transition(ctx context.Context, leadID int64, to int16, actor Actor, reason string) (*models.Lead, error) {
	lead, err := m.leads.GetByID(ctx, leadID)
	if err != nil {
		return nil, fmt.Errorf("kanban: transition: лид %d: %w", leadID, err)
	}

	if lead.StageID == to {
		// Уже там. Ручной повтор (double click менеджера) TTL не трогает —
		// это не «обновление лида», для сброса TTL у M8 есть ResetTTL.
		// Ретрай же авто-перехода довзводит TTL/снятие antispam-задач.
		if actor == ActorManager {
			return lead, nil
		}
		if err := m.ensureStageEffects(ctx, lead, to, actor); err != nil {
			return nil, err
		}
		return lead, nil
	}

	if !CanTransition(lead.StageID, to, actor) {
		return nil, fmt.Errorf("kanban: %s: %d → %d: %w", actor, lead.StageID, to, ErrInvalidTransition)
	}

	from := lead.StageID
	ok, err := m.leads.TransitionStage(ctx, leadID, from, to)
	if err != nil {
		return nil, fmt.Errorf("kanban: transition %d → %d: %w", from, to, err)
	}
	if !ok {
		// Guard WHERE stage_id=from не совпал: между чтением и записью лида
		// передвинул кто-то ещё. Приоритет §3.1 обеспечен: свой переход
		// НЕ применяем. Manual/Payment получают конфликт наверх (409 в M8).
		return nil, fmt.Errorf("kanban: %s: %d → %d: %w", actor, from, to, ErrStageConflict)
	}
	lead.StageID = to
	lead.AntiSpamCount = 0

	if err := m.ensureStageEffects(ctx, lead, to, actor); err != nil {
		return nil, err
	}

	oldStage := from
	m.publish(ctx, events.Event{
		Type:       events.TypeStageChange,
		LeadID:     lead.ID,
		StageID:    to,
		OldStageID: &oldStage,
		Actor:      string(actor),
		Reason:     reason,
	})
	m.log.Info("kanban: переход стадии",
		"lead_id", lead.ID, "from", from, "to", to, "actor", actor, "reason", reason)
	return lead, nil
}

// ensureStageEffects — идемпотентные побочные эффекты пребывания в стадии to:
// снятие anti-spam задач (счётчик уже сброшен CAS-ом, §3.5) и TTL по §3.4.
func (m *Machine) ensureStageEffects(ctx context.Context, lead *models.Lead, to int16, actor Actor) error {
	if err := m.antiSpam.Cancel(ctx, lead.ID); err != nil {
		return fmt.Errorf("kanban: снятие anti-spam задач: %w", err)
	}
	if actor == ActorTTL {
		// Переход инициирован самой задачей ttl:expire — она сейчас active,
		// а DeleteTask активной задачи в asynq это FailedPrecondition-ошибка.
		// Задача исчезнет сама по завершении обработчика; чистим только
		// leads.ttl_task_id. Новый TTL не нужен: TTL-переходы ведут только
		// в Stage 8 (§3.4), у него TTL нет.
		if err := m.leads.UpdateFields(ctx, lead.ID,
			map[string]interface{}{"ttl_task_id": nil}); err != nil {
			return fmt.Errorf("kanban: очистка ttl_task_id: %w", err)
		}
		return nil
	}
	return m.resetTTL(ctx, lead.ID, to)
}

// resetTTL взводит TTL стадии заново (DeleteTask + новый enqueue внутри
// TTLManager.Schedule, CLAUDE.md §4.7) либо снимает его, если стадия без TTL.
func (m *Machine) resetTTL(ctx context.Context, leadID int64, stage int16) error {
	delay, has := m.ttlDelay(stage)
	var err error
	if has {
		err = m.ttl.Schedule(ctx, leadID, delay)
	} else {
		err = m.ttl.Cancel(ctx, leadID)
	}
	if err != nil && !errors.Is(err, queue.ErrDuplicate) {
		return fmt.Errorf("kanban: ttl стадии %d: %w", stage, err)
	}
	return nil
}

// ttlDelay — §3.4: стадии с TTL и его длительность (config §12: kanban.*).
func (m *Machine) ttlDelay(stage int16) (time.Duration, bool) {
	switch stage {
	case StageUnpaid:
		return time.Duration(m.cfg.TTLStage4Hours) * time.Hour, true
	case StageProposal:
		return 24 * time.Duration(m.cfg.TTLStage6Days) * time.Hour, true
	default:
		return 0, false
	}
}

// ResetTTL — ручное обновление лида менеджером (IQ-4): re-anchor TTL от
// текущего момента. last_activity_at тоже обновляется — TTL всегда считается
// от него (CLAUDE.md §4.7), поле и task обязаны смотреть в одну точку.
// Контракт для M8 (PATCH карточки лида без смены стадии).
func (m *Machine) ResetTTL(ctx context.Context, leadID int64) error {
	lead, err := m.leads.GetByID(ctx, leadID)
	if err != nil {
		return fmt.Errorf("kanban: reset ttl: лид %d: %w", leadID, err)
	}
	if _, has := m.ttlDelay(lead.StageID); !has {
		return nil // стадия без TTL — сбрасывать нечего
	}
	if err := m.leads.UpdateFields(ctx, leadID,
		map[string]interface{}{"last_activity_at": time.Now().UTC()}); err != nil {
		return fmt.Errorf("kanban: reset ttl: last_activity_at: %w", err)
	}
	return m.resetTTL(ctx, leadID, lead.StageID)
}

// OnInbound — авто-триггеры на каждое inbound-сообщение (зовёт воркер после
// загрузки лида; счётчики уже увеличены транзакцией CreateInbound в M2).
//
// Порядок существенен:
//  1. авто-переход 1→2 по счётчику (§3.2) — он сбрасывает anti_spam_count,
//     поэтому проверка лимита ниже идёт по состоянию ПОСЛЕ перехода;
//  2. активность лида в стадии с TTL re-anchor'ит TTL (§3.4: от last_activity_at,
//     только что обновлённого CreateInbound);
//  3. anti-spam §3.5: на лимите бот замолкает (silenced=true — воркер НЕ зовёт
//     Claude и не отвечает), менеджеру уходит antispam_alert, взводятся
//     followup (24ч) и escalate (48ч).
//
// lead мутируется по факту авто-перехода (стадия, счётчик).
func (m *Machine) OnInbound(ctx context.Context, lead *models.Lead) (silenced bool, err error) {
	// 1) §3.2: шестое inbound делает лида «живым».
	if lead.StageID == StageGrey && lead.MessageCount >= AutoAdvanceInbound {
		fresh, err := m.Transition(ctx, lead.ID, StageLive, ActorSystem,
			fmt.Sprintf("message_count=%d (>=%d)", lead.MessageCount, AutoAdvanceInbound))
		switch {
		case errors.Is(err, ErrStageConflict):
			// Кто-то (менеджер/оплата) передвинул лида параллельно — их
			// приоритет выше (§3.1), работаем с фактическим состоянием.
			if fresh, err = m.leads.GetByID(ctx, lead.ID); err != nil {
				return false, fmt.Errorf("kanban: on inbound: перечитка лида: %w", err)
			}
		case err != nil:
			return false, fmt.Errorf("kanban: on inbound: авто-переход 1→2: %w", err)
		}
		*lead = *fresh
	} else if _, has := m.ttlDelay(lead.StageID); has {
		// 2) §3.4/§4.7: inbound = активность, TTL пересчитывается от неё.
		// (В ветке авто-перехода TTL уже перевзвёл Transition.)
		if err := m.resetTTL(ctx, lead.ID, lead.StageID); err != nil {
			return false, fmt.Errorf("kanban: on inbound: %w", err)
		}
	}

	// 3) §3.5: лимит inbound на стадию.
	limit := m.cfg.AntiSpamLimit
	if limit <= 0 || lead.AntiSpamCount < limit {
		return false, nil
	}
	// Задачи взводятся ровно на пороге: n-е сообщение ПОСЛЕ лимита не должно
	// перевзводить followup, уже отработавший свои 24ч, — follow-up один (§3.5).
	if lead.AntiSpamCount == limit {
		fresh, err := m.antiSpam.Schedule(ctx, lead.ID, lead.StageID,
			time.Duration(m.cfg.AntiSpamFollowupHours)*time.Hour,
			time.Duration(m.cfg.AntiSpamEscalateHours)*time.Hour)
		if err != nil {
			return true, fmt.Errorf("kanban: on inbound: взвод anti-spam задач: %w", err)
		}
		// fresh=false — конкурент/ретрай уже взвёл и опубликовал alert.
		if fresh {
			m.publish(ctx, events.Event{
				Type:          events.TypeAntiSpamAlert,
				LeadID:        lead.ID,
				StageID:       lead.StageID,
				AntiSpamCount: lead.AntiSpamCount,
				Reason:        "anti-spam limit reached, bot muted",
			})
			m.log.Warn("kanban: anti-spam лимит, бот замолкает",
				"lead_id", lead.ID, "stage_id", lead.StageID, "anti_spam_count", lead.AntiSpamCount)
		}
	}
	return true, nil
}

// HandleTTLExpire — бизнес-логика задачи ttl:expire (контракт M3 §6.4):
// стадия с TTL истекла без активности → Stage 8 (§3.4).
func (m *Machine) HandleTTLExpire(ctx context.Context, leadID int64) error {
	lead, err := m.leads.GetByID(ctx, leadID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil // лид стёрт (LGPD) — задача неактуальна
	}
	if err != nil {
		return fmt.Errorf("kanban: ttl expire: лид %d: %w", leadID, err)
	}
	if _, has := m.ttlDelay(lead.StageID); !has && lead.StageID != StageFailed {
		// Лид уже не в TTL-стадии, а задача не снялась (гонка с Cancel) —
		// guard поверх Cancel: молча выходим, ничего не архивируем.
		// StageFailed — исключение: это ретрай нас же после падения side
		// effects, Transition ниже идемпотентно доведёт их (ветка «уже в to»).
		m.log.Info("kanban: ttl сработал для лида вне TTL-стадии, пропуск",
			"lead_id", leadID, "stage_id", lead.StageID)
		return nil
	}
	_, err = m.Transition(ctx, leadID, StageFailed, ActorTTL,
		fmt.Sprintf("ttl стадии %d истёк", lead.StageID))
	if errors.Is(err, ErrStageConflict) {
		return nil // конкурент передвинул лида — его приоритет (§3.1)
	}
	return err
}

// HandleAntiSpamFollowup — задача antispam:followup (§3.5): спустя 24ч
// молчания бот отправляет ОДНО follow-up-сообщение.
func (m *Machine) HandleAntiSpamFollowup(ctx context.Context, p queue.AntiSpamPayload) error {
	lead, ok, err := m.antiSpamTaskLead(ctx, p)
	if err != nil || !ok {
		return err
	}
	// Порядок «save → send» как в основном пайплайне (§6.2): при падении
	// Send ретрай увидит сохранённый outbound... но здесь нет ветки
	// переотправки, поэтому наоборот — send → save: недоставленное
	// сообщение ретраится, а дубль строки в messages не критичен
	// (счётчики outbound не двигает, CLAUDE.md §4.3).
	if err := m.sender.Send(lead.TelegramUserID, followupText); err != nil {
		return fmt.Errorf("kanban: followup send: %w", err)
	}
	if err := m.msgs.CreateOutbound(ctx, &models.Message{LeadID: lead.ID, Content: followupText}); err != nil {
		return fmt.Errorf("kanban: followup save outbound: %w", err)
	}
	m.log.Info("kanban: anti-spam follow-up отправлен", "lead_id", lead.ID, "stage_id", lead.StageID)
	return nil
}

// HandleAntiSpamEscalate — задача antispam:escalate (§3.5, AQ²-fix #8):
// 48ч молчания → manager_escalation + leads.escalated_at. Гарантия
// критерия приёмки «лид не застревает навсегда»: даже без реакции
// менеджера на alert эскалация случается автоматически.
func (m *Machine) HandleAntiSpamEscalate(ctx context.Context, p queue.AntiSpamPayload) error {
	lead, ok, err := m.antiSpamTaskLead(ctx, p)
	if err != nil || !ok {
		return err
	}
	if err := m.leads.UpdateFields(ctx, lead.ID,
		map[string]interface{}{"escalated_at": time.Now().UTC()}); err != nil {
		return fmt.Errorf("kanban: escalate: запись escalated_at: %w", err)
	}
	m.publish(ctx, events.Event{
		Type:          events.TypeManagerEscalation,
		LeadID:        lead.ID,
		StageID:       lead.StageID,
		AntiSpamCount: lead.AntiSpamCount,
		Reason:        "48h anti-spam silence, manager escalation",
	})
	m.log.Warn("kanban: anti-spam эскалация на менеджера",
		"lead_id", lead.ID, "stage_id", lead.StageID)
	return nil
}

// antiSpamTaskLead — общий guard отложенных anti-spam задач: лид существует,
// всё ещё в стадии из payload и всё ещё замолчан. Иначе задача устарела
// (стадия сменилась в окно между срабатыванием и Cancel) — тихий no-op.
func (m *Machine) antiSpamTaskLead(ctx context.Context, p queue.AntiSpamPayload) (*models.Lead, bool, error) {
	lead, err := m.leads.GetByID(ctx, p.LeadID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, false, nil // LGPD erasure
	}
	if err != nil {
		return nil, false, fmt.Errorf("kanban: anti-spam задача: лид %d: %w", p.LeadID, err)
	}
	if lead.StageID != p.StageID || lead.AntiSpamCount < m.cfg.AntiSpamLimit {
		m.log.Info("kanban: anti-spam задача устарела, пропуск",
			"lead_id", p.LeadID, "task_stage", p.StageID,
			"stage_id", lead.StageID, "anti_spam_count", lead.AntiSpamCount)
		return nil, false, nil
	}
	return lead, true, nil
}

// publish — fire-and-forget PUBLISH crm:events (§10.1): ошибка логируется,
// но бизнес-операцию не роняет — пропущенное событие клиент добирает
// catch-up'ом §10.3.
func (m *Machine) publish(ctx context.Context, ev events.Event) {
	if err := m.pub.Publish(ctx, ev); err != nil {
		m.log.Warn("kanban: событие не опубликовано",
			"type", ev.Type, "lead_id", ev.LeadID, "error", err)
	}
}
