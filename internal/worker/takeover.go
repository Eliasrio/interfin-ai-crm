// takeover.go — обработчики контура «клиент не висит без ответа» (M13,
// LOGIC-01, тайминги владельца 10+10):
//
//	takeover:reminder — reminder_minutes тишины при менеджере → уведомление
//	    менеджеру в Telegram + WS takeover_reminder + взвод takeover:pickup;
//	takeover:pickup   — ещё pickup_minutes без ответа → Эмма подхватывает:
//	    dialog_mode='bot', пауза снята, enqueue process:inbound (ответ идёт
//	    штатным контуром M3), уведомление менеджеру + WS dialog_mode.
//
// Обе задачи самогасящиеся (no-op), если менеджер уже ответил после
// инициирующего inbound или Эмма снова активна — устаревшие задачи от
// предыдущих сообщений клиента умирают сами, отмена через inspector не нужна.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/repo"
	"github.com/interfin/interfin-ai-crm/internal/settings"
)

// TakeoverDeps — зависимости обработчиков takeover:*.
type TakeoverDeps struct {
	Leads      repo.LeadRepo
	Msgs       repo.MessageRepo
	Settings   settings.Reader
	Enq        queue.TakeoverEnqueuer // взвод takeover:pickup из reminder
	InboundEnq queue.Enqueuer         // enqueue process:inbound при подхвате
	Sender     Sender                 // уведомления менеджеру (контур алертов M3/M11)
	// ManagerChatID — Telegram-чат менеджера (cfg.Telegram.ManagerChatID).
	// 0 = уведомления только в лог, как у алертов dead letter.
	ManagerChatID int64
	// PublicURL — базовый адрес CRM для deep-link на карточку лида в
	// напоминании (config.Telegram.PublicBaseURL); "" = без ссылки.
	PublicURL string
	// Panel — строковые ключи панели (emma_panel.manager_mention для
	// упоминания в напоминаниях); nil — без упоминания (юнит-тесты M13).
	Panel PanelSettings
	Pub           events.Publisher
	Log           *slog.Logger
}

// TakeoverHandlers — обработчики takeover:reminder / takeover:pickup.
type TakeoverHandlers struct {
	deps TakeoverDeps
}

func NewTakeoverHandlers(deps TakeoverDeps) *TakeoverHandlers {
	return &TakeoverHandlers{deps: deps}
}

// HandleReminder — handler takeover:reminder. Ошибка = ретрай Asynq
// (проверки no-op идемпотентны), битый payload — SkipRetry.
func (h *TakeoverHandlers) HandleReminder(ctx context.Context, t *asynq.Task) error {
	d := h.deps
	p, err := takeoverPayload(t)
	if err != nil {
		return err
	}
	log := d.Log.With("lead_id", p.LeadID, "task", queue.TypeTakeoverReminder)

	lead, ok, err := h.waitingLead(ctx, p, log)
	if err != nil || !ok {
		return err
	}

	// Порядок: сперва взводится подхват и следующий повтор, потом
	// уведомление. Упавший шаг ретраит задачу целиком — повторные взводы
	// гасятся ErrDuplicate, а подхват уже гарантирован.
	pickupMin := d.Settings.Minutes(ctx, settings.KeyPickupMinutes)
	err = d.Enq.EnqueueTakeoverPickup(ctx, p, time.Duration(pickupMin)*time.Minute)
	if err != nil && !errors.Is(err, queue.ErrDuplicate) {
		return fmt.Errorf("worker: takeover: постановка подхвата: %w", err)
	}

	// Эскалация: повторное напоминание каждые repeat_minutes, пока очередной
	// повтор успевает до подхвата Эммой (отсчёт от ПЕРВОГО напоминания —
	// подхват взводится при нём же). Ответ менеджера гасит повтор в
	// waitingLead, смена настроек между повторами даёт приближение — ок.
	repeatMin := d.Settings.Minutes(ctx, settings.KeyReminderRepeatMinutes)
	if repeatMin > 0 && (p.Repeat+1)*repeatMin < pickupMin {
		next := p
		next.Repeat++
		err = d.Enq.EnqueueTakeoverReminder(ctx, next, time.Duration(repeatMin)*time.Minute)
		if err != nil && !errors.Is(err, queue.ErrDuplicate) {
			return fmt.Errorf("worker: takeover: постановка повторного напоминания: %w", err)
		}
	}

	waiting := waitingMinutes(p.InboundAt)
	var mention string
	if d.Panel != nil {
		mention = mentionPrefix(d.Panel.String(ctx, settings.KeyManagerMention))
	}
	remind := mention + fmt.Sprintf(
		"⏰ CRM: лид #%d ждёт ответа менеджера уже %d мин (Эмма молчит: диалог взят в работу или на паузе).\nОтветьте из карточки — иначе Эмма подхватит сама.",
		p.LeadID, waiting)
	if link := leadCardURL(d.PublicURL, p.LeadID); link != "" {
		remind += "\nОткрыть диалог: " + link
	}
	h.notifyManager(remind, log)

	h.publish(ctx, events.Event{
		Type:           events.TypeTakeoverReminder,
		LeadID:         lead.ID,
		StageID:        lead.StageID,
		WaitingMinutes: waiting,
		Reason:         "manager silent after inbound",
	}, log)

	log.Info("worker: напоминание менеджеру отправлено",
		"waiting_minutes", waiting, "repeat", p.Repeat, "pickup_minutes", pickupMin)
	return nil
}

// HandlePickup — handler takeover:pickup: менеджер так и не ответил —
// Эмма забирает диалог обратно и отвечает штатным контуром process:inbound.
func (h *TakeoverHandlers) HandlePickup(ctx context.Context, t *asynq.Task) error {
	d := h.deps
	p, err := takeoverPayload(t)
	if err != nil {
		return err
	}
	log := d.Log.With("lead_id", p.LeadID, "task", queue.TypeTakeoverPickup)

	lead, ok, err := h.waitingLead(ctx, p, log)
	if err != nil || !ok {
		return err
	}

	// Возврат к Эмме: режим bot, пауза и владелец снимаются одной командой.
	err = d.Leads.UpdateFields(ctx, lead.ID, map[string]interface{}{
		"dialog_mode":        models.DialogModeBot,
		"bot_silenced_until": nil,
		"taken_by":           nil,
	})
	if errors.Is(err, repo.ErrNotFound) {
		log.Warn("worker: лид стёрт, подхват не нужен")
		return nil
	}
	if err != nil {
		return fmt.Errorf("worker: takeover: возврат режима bot: %w", err)
	}
	lead.DialogMode = models.DialogModeBot
	lead.BotSilencedUntil = nil
	lead.TakenBy = nil

	// Ответ клиенту — штатным контуром M3: тот же telegram message_id даёт
	// боевой дедуп-ключ; повтор (ретрай после падения ниже) гасится сам.
	if err := d.InboundEnq.EnqueueInbound(ctx, lead.ID, p.TgMsgID); err != nil &&
		!errors.Is(err, queue.ErrDuplicate) {
		return fmt.Errorf("worker: takeover: enqueue process:inbound: %w", err)
	}

	h.publish(ctx, events.DialogModeEvent(lead, events.ReasonTakeoverPickup), log)
	h.notifyManager(fmt.Sprintf(
		"🤖 CRM: Эмма подхватила лид #%d — менеджер не ответил вовремя. Диалог снова у бота.",
		lead.ID), log)

	log.Info("worker: Эмма подхватила диалог — менеджер не ответил")
	return nil
}

// waitingLead — общие проверки no-op обеих задач: лид жив, Эмма всё ещё
// молчит (human/активная пауза), менеджер НЕ ответил после инициирующего
// inbound. ok=false без ошибки — задача неактуальна, гаснет молча.
func (h *TakeoverHandlers) waitingLead(ctx context.Context, p queue.TakeoverPayload, log *slog.Logger) (*models.Lead, bool, error) {
	lead, err := h.deps.Leads.GetByID(ctx, p.LeadID)
	if errors.Is(err, repo.ErrNotFound) {
		log.Info("worker: лид стёрт — задача takeover неактуальна")
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("worker: takeover: загрузка лида: %w", err)
	}
	if !lead.BotSilenced(time.Now()) {
		// Режим снова bot (вернули кнопкой, пауза истекла или подхват уже
		// случился) — Эмма активна, контур напоминаний не нужен.
		log.Info("worker: Эмма снова активна — задача takeover гаснет")
		return nil, false, nil
	}
	replied, err := h.deps.Msgs.HasManagerOutboundAfter(ctx, lead.ID, p.InboundMsgID)
	if err != nil {
		return nil, false, fmt.Errorf("worker: takeover: проверка ответа менеджера: %w", err)
	}
	if replied {
		log.Info("worker: менеджер уже ответил — задача takeover гаснет")
		return nil, false, nil
	}
	return lead, true, nil
}

// notifyManager — уведомление в Telegram-чат менеджера. Best effort ПОСЛЕ
// гарантированных шагов: недоставленное уведомление не роняет задачу
// (ретрай продублировал бы взведённые шаги ради текста в чате).
func (h *TakeoverHandlers) notifyManager(text string, log *slog.Logger) {
	if h.deps.ManagerChatID == 0 {
		return // чат менеджера не сконфигурирован — остаёмся при логе
	}
	if err := h.deps.Sender.Send(h.deps.ManagerChatID, text); err != nil {
		log.Error("worker: уведомление менеджеру не доставлено", "error", err)
	}
}

// publish — fire-and-forget, как весь канал crm:events: пропуск фронт
// добирает перезапросом (§10.3).
func (h *TakeoverHandlers) publish(ctx context.Context, ev events.Event, log *slog.Logger) {
	if h.deps.Pub == nil {
		return
	}
	if err := h.deps.Pub.Publish(ctx, ev); err != nil {
		log.Warn("worker: событие "+ev.Type+" не опубликовано", "error", err)
	}
}

// waitingMinutes — сколько минут клиент ждёт ответа (для текста уведомления
// и WS-события), не меньше 1: событие «ждёт 0 минут» читалось бы как мусор.
func waitingMinutes(inboundAt time.Time) int {
	if inboundAt.IsZero() {
		return 1
	}
	m := int(time.Since(inboundAt) / time.Minute)
	if m < 1 {
		m = 1
	}
	return m
}

func takeoverPayload(t *asynq.Task) (queue.TakeoverPayload, error) {
	var p queue.TakeoverPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return p, fmt.Errorf("worker: payload %s не разобран: %v: %w", t.Type(), err, asynq.SkipRetry)
	}
	return p, nil
}
