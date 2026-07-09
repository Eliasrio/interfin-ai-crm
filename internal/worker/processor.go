// processor.go — обработчик process:inbound: Worker Pipeline SRS §6.2.
//
//  1. Typing  2) RAG retrieval (M4 §7.1)  3) Контекст (token budget, summary)
//  4. Anthropic /v1/messages  5) Save outbound  6) bot.Send
//     (7: PUBLISH crm:events — появится в M5)
//
// Идемпотентность (§6.3): дубли задач гасит TaskID+Unique ещё на enqueue (M2);
// здесь — идемпотентность РЕТРАЯ. Порядок «save → send» означает, что при
// падении Send ответ уже в БД: повторная попытка видит последним outbound
// и только переотправляет его, НЕ вызывая Claude и не плодя строк в messages.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/claude"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/repo"
	"github.com/interfin/interfin-ai-crm/internal/settings"
)

// historyFetchLimit — сколько последних сообщений поднимать из БД.
// Дальше окно всё равно режет токен-бюджетер (history ≤ 4000 токенов §7.2).
const historyFetchLimit = 50

// Sender — отправка в Telegram (в проде обёртка над telebot, в тестах фейк).
type Sender interface {
	// Typing показывает лиду «печатает…» (§6.2 шаг 1). Best effort.
	Typing(chatID int64) error
	// Send отправляет текст в чат.
	Send(chatID int64, text string) error
}

// Completer — вызов Claude (в проде *claude.Client, в тестах фейк).
type Completer interface {
	Complete(ctx context.Context, system string, msgs []claude.Message) (string, error)
}

// Retriever — RAG-поиск по базе знаний (в проде *rag.Retriever, в тестах фейк).
type Retriever interface {
	Retrieve(ctx context.Context, leadID int64, query string) ([]repo.ScoredChunk, error)
}

// ProcessorDeps — зависимости обработчика process:inbound. Retriever,
// Summaries и SummaryEnq допускают nil (диалог без RAG/summary) — это
// режим юнит-тестов M3; боевая сборка (cmd/server) задаёт всё.
type ProcessorDeps struct {
	Leads    repo.LeadRepo
	Msgs     repo.MessageRepo
	Budgeter *Budgeter
	AI       Completer
	Sender   Sender

	// M4 (RAG + summary):
	Retriever     Retriever
	Summaries     repo.SummaryRepo
	SummaryEnq    queue.SummaryEnqueuer
	SummaryEveryN int // kanban.summary_every_n_messages (§7.3: 15)

	// M5 (state machine): авто-триггеры §3.2/§3.4/§3.5 на каждый inbound.
	// nil — режим юнит-тестов M3 (диалог без Kanban-логики).
	Kanban StateMachine

	// M12: событие message на каждый сохранённый outbound (чат менеджера
	// live). nil — без публикации (юнит-тесты M3).
	Pub events.Publisher

	// M13 (takeover): Settings — интервалы контура напоминаний;
	// TakeoverEnq — постановка takeover:reminder на inbound при молчащей
	// Эмме. Оба nil — режим юнит-тестов M3 (без контура напоминаний).
	Settings    settings.Reader
	TakeoverEnq queue.TakeoverEnqueuer

	Log *slog.Logger
}

// Processor — обработчик process:inbound.
type Processor struct {
	deps ProcessorDeps
}

func NewProcessor(deps ProcessorDeps) *Processor {
	return &Processor{deps: deps}
}

// HandleProcessInbound — handler задачи process:inbound (контракт M2→M3).
// Возврат ошибки = ретрай Asynq (3×, backoff 2/8/32с — server.go), затем
// dead letter + алерт (§6.3). Возврат nil = задача выполнена.
func (p *Processor) HandleProcessInbound(ctx context.Context, t *asynq.Task) error {
	d := p.deps
	var payload queue.InboundPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		// Битый payload не починится ретраем — сразу в архив (§6.3).
		return fmt.Errorf("worker: payload process:inbound не разобран: %v: %w", err, asynq.SkipRetry)
	}
	log := d.Log.With("lead_id", payload.LeadID, "tg_msg_id", payload.MsgID)

	lead, err := d.Leads.GetByID(ctx, payload.LeadID)
	if errors.Is(err, repo.ErrNotFound) {
		// Лид стёрт (LGPD erasure) между enqueue и обработкой — задача неактуальна.
		log.Warn("worker: лид не найден, задача пропущена")
		return nil
	}
	if err != nil {
		return fmt.Errorf("worker: загрузка лида: %w", err)
	}

	// M5: авто-триггеры до генерации ответа — переход 1→2 (§3.2), reset TTL
	// по активности (§3.4), anti-spam (§3.5). Замолчанному лиду не отвечаем:
	// ни Claude, ни typing, ни summary (ошибка здесь = ретрай всей задачи,
	// шаги state machine идемпотентны).
	if d.Kanban != nil {
		silenced, err := d.Kanban.OnInbound(ctx, lead)
		if err != nil {
			return fmt.Errorf("worker: kanban on inbound: %w", err)
		}
		if silenced {
			log.Info("worker: anti-spam молчание, ответ не генерируется",
				"stage_id", lead.StageID, "anti_spam_count", lead.AntiSpamCount)
			return nil
		}
	}

	// M13 (проверка №1): диалог у менеджера или идёт пауза автопилота —
	// Эмма молчит. Inbound уже сохранён (M2), Kanban-триггеры отработали
	// выше; вместо ответа взводится контур напоминаний (LOGIC-01), чтобы
	// клиент не повис без ответа. Просроченная пауза = обычный режим bot.
	if lead.BotSilenced(time.Now()) {
		log.Info("worker: Эмма молчит (human/пауза), взводится напоминание",
			"dialog_mode", lead.DialogMode, "silenced_until", lead.BotSilencedUntil)
		return p.armTakeoverReminder(ctx, lead, payload.MsgID, log)
	}

	history, err := d.Msgs.ListByLead(ctx, lead.ID, historyFetchLimit)
	if err != nil {
		return fmt.Errorf("worker: история сообщений: %w", err)
	}
	if len(history) == 0 {
		log.Warn("worker: у лида нет сообщений, отвечать не на что")
		return nil
	}

	// Триггер сводки §7.3 — до генерации ответа: постановка идемпотентна
	// (TaskID по (lead, count)), а так её не теряет ни ретрай, ни ветка
	// переотправки ниже.
	p.maybeEnqueueSummary(ctx, lead, log)

	// Идемпотентность ретрая: последний в истории outbound = ответ уже
	// сгенерирован и сохранён (шаг «save» прошёл), упасть мог только Send.
	// Переотправляем сохранённое, к Claude не ходим.
	if last := history[len(history)-1]; last.Direction == models.DirectionOutbound {
		log.Info("worker: ответ уже сохранён, переотправка без вызова Claude")
		if err := d.Sender.Send(lead.TelegramUserID, last.Content); err != nil {
			return fmt.Errorf("worker: переотправка ответа: %w", err)
		}
		return nil
	}

	// Нетекстовое входящее (голос/стикер/фото → content пуст): Claude звать
	// не с чем — диалог, кончающийся репликой ассистента (или пустой),
	// Sonnet 5 отвергает как prefill (api 400, поймано в проде на
	// голосовом), а бюджетер на истории из одних пустых падает. Отвечаем
	// детерминированной подсказкой тем же контуром save → send: ретрай
	// после падения Send уйдёт в ветку переотправки выше.
	if strings.TrimSpace(history[len(history)-1].Content) == "" {
		// M13 (проверка №2): режим мог смениться, пока задача ждала в
		// очереди/ретраилась — подсказка тоже не перебивает менеджера.
		if dropped, err := p.dropIfSilenced(ctx, lead, payload.MsgID, log); err != nil || dropped {
			return err
		}
		tokens := estimateTokens(nonTextReply)
		author := models.AuthorBot
		out := &models.Message{LeadID: lead.ID, Author: &author, Content: nonTextReply, Tokens: &tokens}
		if err := d.Msgs.CreateOutbound(ctx, out); err != nil {
			return fmt.Errorf("worker: сохранение подсказки о нетекстовом: %w", err)
		}
		p.publishMessage(ctx, out, lead.StageID, log)
		if err := d.Sender.Send(lead.TelegramUserID, nonTextReply); err != nil {
			return fmt.Errorf("worker: отправка подсказки о нетекстовом: %w", err)
		}
		log.Info("worker: нетекстовое входящее — отправлена подсказка, Claude не вызывался")
		return nil
	}

	// 1) Typing — best effort: недоставленный индикатор не стоит ретрая.
	if err := d.Sender.Typing(lead.TelegramUserID); err != nil {
		log.Warn("worker: typing не отправлен", "error", err)
	}

	// 2) RAG §7.1: знания под последний вопрос лида. Ошибка Voyage/БД —
	// транзиент, лечится ретраем; rag_miss — НЕ ошибка (fallback внутри
	// ретривера уже оставил след в rag_audit), диалог идёт без чанков.
	var chunks []repo.ScoredChunk
	if query := strings.TrimSpace(history[len(history)-1].Content); d.Retriever != nil && query != "" {
		if chunks, err = d.Retriever.Retrieve(ctx, lead.ID, query); err != nil {
			return fmt.Errorf("worker: rag retrieval: %w", err)
		}
	}

	// 3) Контекст в пределах бюджета §7.2: system+RAG ≤ 2000, summary ≤ 1000.
	summary := p.loadSummary(ctx, lead.ID, log)
	system, msgs, stats, err := d.Budgeter.Build(ctx, composeSystemPrompt(systemPrompt, chunks), summary, history)
	if err != nil {
		return fmt.Errorf("worker: сборка контекста: %w", err)
	}

	// 4) Claude. Ошибка (5xx, rate limit, сеть) → ретрай, затем dead letter.
	reply, err := d.AI.Complete(ctx, system, msgs)
	if err != nil {
		return fmt.Errorf("worker: вызов claude: %w", err)
	}

	// M13, BUG-01 (проверка №2): менеджер мог забрать диалог за время
	// генерации (3–15 с). Режим перечитывается из БД НЕПОСРЕДСТВЕННО перед
	// сохранением: сменился — ответ отбрасывается без записи и без Send
	// (несохранённое не переотправится и веткой ретрая выше).
	if dropped, err := p.dropIfSilenced(ctx, lead, payload.MsgID, log); err != nil || dropped {
		return err
	}

	// 5) Save outbound. Счётчики лида НЕ трогаем (CLAUDE.md §4.3).
	replyTokens := estimateTokens(reply)
	author := models.AuthorBot
	out := &models.Message{LeadID: lead.ID, Author: &author, Content: reply, Tokens: &replyTokens}
	if err := d.Msgs.CreateOutbound(ctx, out); err != nil {
		return fmt.Errorf("worker: сохранение ответа: %w", err)
	}
	// M12: событие message сразу после save, а не после send: ветка
	// переотправки (ретрай упавшего Send) второй раз НЕ публикует.
	p.publishMessage(ctx, out, lead.StageID, log)

	// 6) Send. При падении ретрай уйдёт в ветку переотправки выше.
	if err := d.Sender.Send(lead.TelegramUserID, reply); err != nil {
		return fmt.Errorf("worker: отправка ответа: %w", err)
	}

	log.Info("worker: ответ отправлен",
		"rag_chunks", len(chunks),
		"summary_used", summary != "",
		"estimate_tokens", stats.EstimateTokens,
		"exact_tokens", stats.ExactTokens,
		"count_tokens_calls", stats.ExactCalls,
		"dropped_history", stats.DroppedHistory,
		"reply_tokens_estimate", replyTokens,
	)
	return nil
}

// dropIfSilenced — вторая проверка режима (M13, BUG-01): перечитывает лида
// из БД перед CreateOutbound. dropped=true — режим сменился на human/паузу,
// ответ отброшен, задача завершена успешно (не ретрай); заодно взводится
// контур напоминаний — inbound остался без ответа при менеджере.
// Стёртый за время генерации лид тоже гасит задачу.
func (p *Processor) dropIfSilenced(ctx context.Context, lead *models.Lead, tgMsgID int, log *slog.Logger) (bool, error) {
	fresh, err := p.deps.Leads.GetByID(ctx, lead.ID)
	if errors.Is(err, repo.ErrNotFound) {
		log.Warn("worker: лид стёрт за время генерации, ответ отброшен")
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("worker: перечитывание режима перед ответом: %w", err)
	}
	if !fresh.BotSilenced(time.Now()) {
		return false, nil
	}
	log.Info("worker: режим сменился за время генерации — ответ отброшен без записи",
		"dialog_mode", fresh.DialogMode, "silenced_until", fresh.BotSilencedUntil)
	return true, p.armTakeoverReminder(ctx, fresh, tgMsgID, log)
}

// armTakeoverReminder ставит takeover:reminder через reminder_minutes
// (LOGIC-01): клиент написал, а Эмма молчит — менеджер обязан ответить,
// иначе придёт напоминание, а затем подхват. Дедуп — TaskID по
// (lead, message_count): повторная доставка апдейта не плодит напоминаний.
// Если менеджер УЖЕ ответил (последнее сообщение — не inbound), напоминание
// не нужно. Ошибка постановки возвращается наверх: ретрай задачи повторит
// взвод — потерянное напоминание оставило бы клиента висеть.
func (p *Processor) armTakeoverReminder(ctx context.Context, lead *models.Lead, tgMsgID int, log *slog.Logger) error {
	d := p.deps
	if d.TakeoverEnq == nil || d.Settings == nil {
		return nil // юнит-тесты M3: контур напоминаний не собран
	}
	last, err := d.Msgs.ListByLead(ctx, lead.ID, 1)
	if err != nil {
		return fmt.Errorf("worker: takeover: последнее сообщение: %w", err)
	}
	if len(last) == 0 || last[0].Direction != models.DirectionInbound {
		return nil // менеджер/бот уже ответил — клиент не ждёт
	}
	delay := time.Duration(d.Settings.Minutes(ctx, settings.KeyReminderMinutes)) * time.Minute
	err = d.TakeoverEnq.EnqueueTakeoverReminder(ctx, queue.TakeoverPayload{
		LeadID:       lead.ID,
		MessageCount: lead.MessageCount,
		InboundMsgID: last[0].ID,
		InboundAt:    last[0].CreatedAt,
		TgMsgID:      tgMsgID,
	}, delay)
	switch {
	case errors.Is(err, queue.ErrDuplicate):
		return nil // уже взведено (дубль апдейта) — §4.5 в работе
	case err != nil:
		return fmt.Errorf("worker: takeover: постановка напоминания: %w", err)
	}
	log.Info("worker: взведено напоминание менеджеру", "delay", delay.String())
	return nil
}

// publishMessage — событие message (M12) для сохранённого outbound.
// Fire-and-forget: пропуск клиент добирает перезапросом истории (§10.3),
// бизнес-операцию не роняем.
func (p *Processor) publishMessage(ctx context.Context, msg *models.Message, stageID int16, log *slog.Logger) {
	if p.deps.Pub == nil {
		return
	}
	if err := p.deps.Pub.Publish(ctx, events.MessageEvent(msg, stageID)); err != nil {
		log.Warn("worker: событие message не опубликовано", "error", err)
	}
}

// loadSummary достаёт сводку диалога (§7.3) для инжекта в контекст.
// Отсутствие сводки — норма (молодой диалог); ошибка БД деградирует до
// ответа без сводки, а не до dead letter.
func (p *Processor) loadSummary(ctx context.Context, leadID int64, log *slog.Logger) string {
	if p.deps.Summaries == nil {
		return ""
	}
	s, err := p.deps.Summaries.GetByLead(ctx, leadID)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		return ""
	case err != nil:
		log.Warn("worker: сводка не загружена, отвечаем без неё", "error", err)
		return ""
	}
	return s.Content
}

// maybeEnqueueSummary ставит summary:generate на каждом N-м inbound (§7.3).
// message_count считает только inbound (CLAUDE.md §4.3), поэтому кратность
// проверяется по нему как есть. Неудача постановки не роняет диалог:
// следующий 15-блок сгенерирует сводку заново.
func (p *Processor) maybeEnqueueSummary(ctx context.Context, lead *models.Lead, log *slog.Logger) {
	d := p.deps
	if d.SummaryEnq == nil || d.SummaryEveryN <= 0 {
		return
	}
	if lead.MessageCount == 0 || lead.MessageCount%d.SummaryEveryN != 0 {
		return
	}
	err := d.SummaryEnq.EnqueueSummary(ctx, lead.ID, lead.MessageCount)
	switch {
	case errors.Is(err, queue.ErrDuplicate):
		// Уже стоит (ретрай или дубль апдейта) — это и есть §4.5 в работе.
	case err != nil:
		log.Warn("worker: задача summary не поставлена", "error", err)
	default:
		log.Info("worker: поставлена задача summary", "message_count", lead.MessageCount)
	}
}
