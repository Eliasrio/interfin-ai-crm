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
	"net"
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
// Расширение интерфейса = правка ВСЕХ тестовых фейков Sender (грабля M14 №3);
// узкие интерфейсы kanban.Sender и handlers (только Send) не задеваются.
type Sender interface {
	// Typing показывает лиду «печатает…» (§6.2 шаг 1). Best effort.
	Typing(chatID int64) error
	// Send отправляет текст в чат.
	Send(chatID int64, text string) error
	// SendDocument отправляет файл документом (EP-04: PDF; контракт
	// EP-05/EP-06). fileName — имя файла, которое увидит клиент.
	SendDocument(chatID int64, path, fileName string) error
	// SendPhoto отправляет изображение фотографией (EP-04: JPG/PNG).
	SendPhoto(chatID int64, path string) error
	// SendWithKeyboard — текст с постоянной reply-клавиатурой из одной
	// кнопки buttonText (EP-05: «Связаться с менеджером»).
	SendWithKeyboard(chatID int64, text, buttonText string) error
	// SendRemoveKeyboard — текст со снятием reply-клавиатуры (EP-05:
	// кнопку выключили в панели).
	SendRemoveKeyboard(chatID int64, text string) error
}

// Completer — вызов Claude (в проде *claude.Client, в тестах фейк).
// Usage — учёт токенов вкладки 6 (EP-06); расширение сигнатуры = правка
// всех тестовых фейков (та же грабля M14 №3, что с Sender).
type Completer interface {
	Complete(ctx context.Context, system string, msgs []claude.Message) (string, claude.Usage, error)
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

	// EP-02: источник системного промпта (панель Эммы, кэш 30 с).
	// nil — фиксированная константа systemPrompt (юнит-тесты M3, поведение
	// до EP-02); боевая сборка задаёт CachedPromptProvider.
	Prompt PromptProvider

	// EP-04 (файлы для отправки). FilesProv — активные файлы для секции
	// system-блока (кэш 30 с); SendFiles — валидация маркеров GetByID
	// (мимо кэша) — оба nil в юнит-тестах M3 (секции нет, маркеры
	// вырезаются, файлы пропускаются). Events — журнал emma_events
	// (file_sent/error), best effort; nil — журнал не пишется.
	FilesProv SendFilesProvider
	SendFiles repo.EmmaSendFilesRepo
	Events    repo.EmmaEventsRepo

	// EP-05 (контакты + сценарий). Contacts — активные контакты для секции
	// system-блока (кэш 30 с); Panel — строковые ключи вкладки 5 (welcome,
	// кнопка менеджера, подтверждение handoff). Оба nil — режим юнит-тестов
	// M3: секции контактов нет, ранний выход панели выключен, ответы уходят
	// старым Send без клавиатуры (поведение до EP-05). ManagerChatID — чат
	// менеджеров для уведомления о client-handoff (канал M13, уточнение ТЗ
	// §4 п.4 — НЕ emma_panel.alert_chat_id); 0 = уведомления только в лог.
	Contacts      ContactsProvider
	Panel         PanelSettings
	ManagerChatID int64

	// EP-06 (алерты): приёмник ошибок/успехов для серий в Redis (в проде
	// *emma.Notifier — шлёт сама Эмма в emma_panel.alert_chat_id). nil —
	// алерты выключены (юнит-тесты M3). Вызовы идут из recordEvent: error →
	// OnError, reply → OnSuccess (сброс серий llm_api/telegram_api).
	Alerts AlertSink

	Log *slog.Logger
}

// AlertSink — контур алертов EP-06 (в проде emma.Notifier, в тестах фейк).
// Оба вызова best effort by design: реализация не возвращает ошибок,
// внутри — только slog (никаких каскадов, task EP-06 §5).
type AlertSink interface {
	// OnError учитывает ошибку kind (llm_api/telegram_api/kb_index — прочие
	// виды игнорируются) и шлёт алерт при выполнении условий серии.
	OnError(ctx context.Context, kind, detail string)
	// OnSuccess сбрасывает серии llm_api/telegram_api (успешный ответ Эммы).
	OnSuccess(ctx context.Context)
}

// PanelSettings — строковые ключи панели Эммы (в проде settings.Service,
// кэш 30 с; в тестах — фейк). Отдельный от settings.Reader контракт:
// Minutes сюда не тянем, фейки M13 не задеваются.
type PanelSettings interface {
	String(ctx context.Context, key string) string
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
	// EP-06: метрика времени ответа — от получения задачи воркером до
	// успешного Send текста (очередь Telegram→вебхук не входит, task §0).
	start := time.Now()
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
		if err := p.sendReply(ctx, lead.TelegramUserID, last.Content); err != nil {
			p.recordSendError(ctx, lead.ID, err, log)
			return fmt.Errorf("worker: переотправка ответа: %w", err)
		}
		// EP-06: reply-событие здесь НЕ пишется (критерий приёмки): Claude
		// не вызывался, usage первой попытки утрачен вместе с упавшим Send.
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
		// M14: подсказка на языке лида (NULL → ru). Ретрай упавшего Send
		// переотправит сохранённый локализованный текст веткой выше.
		reply := nonTextReplyFor(lead.Language)
		tokens := estimateTokens(reply)
		author := models.AuthorBot
		out := &models.Message{LeadID: lead.ID, Author: &author, Content: reply, Tokens: &tokens}
		if err := d.Msgs.CreateOutbound(ctx, out); err != nil {
			return fmt.Errorf("worker: сохранение подсказки о нетекстовом: %w", err)
		}
		p.publishMessage(ctx, out, lead.StageID, log)
		if err := p.sendReply(ctx, lead.TelegramUserID, reply); err != nil {
			return fmt.Errorf("worker: отправка подсказки о нетекстовом: %w", err)
		}
		log.Info("worker: нетекстовое входящее — отправлена подсказка, Claude не вызывался")
		return nil
	}

	// РАННИЙ ВЫХОД ПАНЕЛИ (EP-05, шаг 3 сводной схемы ТЗ) — строго ПОСЛЕ
	// Kanban.OnInbound и проверки режима M13 (QA-фикс: ветка до очереди
	// обошла бы анти-спам и сброс TTL): /start с непустым welcome отвечает
	// без Claude; нажатие кнопки менеджера уводит в handoff.
	if handled, err := p.panelEarlyExit(ctx, lead, &history[len(history)-1], payload.MsgID, log); handled || err != nil {
		return err
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

	// 3) Контекст в пределах бюджета: system+секции панели+RAG ≤ 5000
	// (EP-02, было 2000 по SRS §7.2), summary ≤ 1000. База system-блока —
	// из провайдера промпта (панель Эммы, кэш 30 с; nil/ошибка — константа),
	// секции тем/стиля/языка добавляет buildSystemBase (порядок ТЗ §3).
	summary := p.loadSummary(ctx, lead.ID, log)
	base := buildSystemBase(p.promptConfig(ctx, log), lead.Language,
		p.contactsSection(ctx, log), // EP-05: секция 6 (контакты) — после стиля/языка, перед файлами (ТЗ §3)
		p.filesSection(ctx, log),    // EP-04: секция 7 (файлы)
		handoffInstruction)          // EP-05: маркер {{handoff}} — распознавание просьбы о менеджере
	system, msgs, stats, err := d.Budgeter.Build(ctx, composeSystemPrompt(base, chunks), summary, history)
	if err != nil {
		return fmt.Errorf("worker: сборка контекста: %w", err)
	}

	// 4) Claude. Ошибка (5xx, rate limit, сеть) → ретрай, затем dead letter.
	// EP-06: каждая неудачная попытка оставляет error-событие (llm_api либо
	// timeout — по errors.Is) — так серия из трёх ретраев честно доводит
	// счётчик алертов до порога.
	reply, usage, err := d.AI.Complete(ctx, system, msgs)
	if err != nil {
		p.recordClaudeError(ctx, lead.ID, err, log)
		return fmt.Errorf("worker: вызов claude: %w", err)
	}

	// 4.5) Маркер-протокол (EP-04, ТЗ §3): маркеры вырезаются ДО
	// CreateOutbound — в БД, историю диалога и событие M12 уходит чистый
	// текст. id файлов живут только в памяти обработчика: ветка
	// переотправки выше вложения не восстанавливает (осознанное
	// упрощение v1, ТЗ §3).
	clean, fileIDs, handoff, unknown := ParseMarkers(reply)
	if len(unknown) > 0 {
		log.Warn("worker: нераспознанные маркеры вырезаны из ответа", "markers", unknown)
	}

	// M13, BUG-01 (проверка №2): менеджер мог забрать диалог за время
	// генерации (3–15 с). Режим перечитывается из БД НЕПОСРЕДСТВЕННО перед
	// сохранением: сменился — ответ отбрасывается без записи и без Send
	// (несохранённое не переотправится и веткой ретрая выше).
	if dropped, err := p.dropIfSilenced(ctx, lead, payload.MsgID, log); err != nil || dropped {
		return err
	}

	// 5–6) Save → Send чистого текста. Ответ из одних маркеров легально
	// пуст — Telegram пустой текст не примет, шаг пропускается (файлы ниже
	// оставят свои [файл: …] в истории, ретрай в Claude не пойдёт).
	if clean != "" {
		// Save outbound. Счётчики лида НЕ трогаем (CLAUDE.md §4.3).
		replyTokens := estimateTokens(clean)
		author := models.AuthorBot
		out := &models.Message{LeadID: lead.ID, Author: &author, Content: clean, Tokens: &replyTokens}
		if err := d.Msgs.CreateOutbound(ctx, out); err != nil {
			return fmt.Errorf("worker: сохранение ответа: %w", err)
		}
		// M12: событие message сразу после save, а не после send: ветка
		// переотправки (ретрай упавшего Send) второй раз НЕ публикует.
		p.publishMessage(ctx, out, lead.StageID, log)

		// Send. При падении ретрай уйдёт в ветку переотправки выше.
		if err := p.sendReply(ctx, lead.TelegramUserID, clean); err != nil {
			p.recordSendError(ctx, lead.ID, err, log)
			return fmt.Errorf("worker: отправка ответа: %w", err)
		}
	} else {
		log.Warn("worker: после вырезания маркеров текст пуст, отправляются только файлы",
			"file_ids", fileIDs)
	}

	// EP-06: reply-событие — единственный источник учёта расходов Claude
	// (контракт task §«наружу»: семантику полей не менять без правки stats).
	// Пишется строго ПОСЛЕ успешного Send текста; при пустом clean (ответ из
	// одних маркеров) — сразу: Claude вызван, токены оплачены. Welcome,
	// handoff-подтверждение и подсказка о нетекстовом сюда не попадают —
	// это ветки без Claude, у них нет ни usage, ни reply-события (осознанно).
	rt := int(time.Since(start).Milliseconds())
	if rt == 0 {
		// Колонка целочисленная в мс; у мгновенного фейка в тестах 0 был бы
		// неотличим от «не замерено» — фиксируем минимум.
		rt = 1
	}
	p.recordEvent(ctx, &models.EmmaEvent{
		EventType:      models.EmmaEventReply,
		LeadID:         &lead.ID,
		ResponseTimeMs: &rt,
		TokensIn:       &usage.InputTokens,
		TokensOut:      &usage.OutputTokens,
	}, log)

	// 7) Файлы по маркерам — ПОСЛЕ успешного Send текста (ТЗ §3, сводная
	// схема шаг 5–6). Ошибки внутри не роняют задачу: текст уже ушёл,
	// ретрай продублировал бы ответ клиенту.
	p.sendMarkedFiles(ctx, lead, fileIDs, log)

	// 8) {{handoff}} от Claude (EP-05, ТЗ §4 п.2) — тоже ПОСЛЕ успешного
	// Send: собственный ответ Эммы уже ушёл чистым, confirm-текст вторым
	// сообщением НЕ шлётся. Ошибки не роняют задачу (симметрично файлам):
	// ретрай продублировал бы текст клиенту веткой переотправки.
	if handoff {
		if err := p.clientHandoff(ctx, lead, &history[len(history)-1], payload.MsgID, "", log); err != nil {
			log.Error("worker: handoff по маркеру не выполнен", "error", err)
		}
	}

	replyTokens := estimateTokens(clean)
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

// panelScenario — снимок ключей вкладки 5 (EP-05) на один шаг обработки.
// Тексты тримятся здесь один раз; кнопка с пустым текстом считается
// выключенной (API такое не сохранит — страховка от ручной правки БД).
type panelScenario struct {
	welcome       string
	buttonEnabled bool
	buttonText    string
	confirmText   string
}

// scenario — чтение ключей вкладки 5 из settings (кэш 30 с — правка панели
// доезжает без рестарта). Panel nil — нулевой сценарий: welcome пуст,
// кнопка выключена (поведение до EP-05).
func (p *Processor) scenario(ctx context.Context) panelScenario {
	if p.deps.Panel == nil {
		return panelScenario{}
	}
	sc := panelScenario{
		welcome:       strings.TrimSpace(p.deps.Panel.String(ctx, settings.KeyWelcomeText)),
		buttonEnabled: p.deps.Panel.String(ctx, settings.KeyManagerButtonEnabled) == "true",
		buttonText:    strings.TrimSpace(p.deps.Panel.String(ctx, settings.KeyManagerButtonText)),
		confirmText:   strings.TrimSpace(p.deps.Panel.String(ctx, settings.KeyHandoffConfirmText)),
	}
	if sc.buttonText == "" {
		sc.buttonEnabled = false
	}
	return sc
}

// sendReply — отправка текста лиду с ветвлением клавиатуры (EP-05, ТЗ §3
// вкладка 5): кнопка включена → КАЖДЫЙ ответ Эммы уходит с клавиатурой;
// выключена → с RemoveKeyboard (иначе кнопка осталась бы у клиента
// навсегда; состояние «надо снять» — по settings, без новых полей в БД,
// для чата без клавиатуры это no-op Telegram). Panel nil (юнит-тесты M3,
// e2e старого контура) — старый Send без клавиатуры.
func (p *Processor) sendReply(ctx context.Context, chatID int64, text string) error {
	if p.deps.Panel == nil {
		return p.deps.Sender.Send(chatID, text)
	}
	if sc := p.scenario(ctx); sc.buttonEnabled {
		return p.deps.Sender.SendWithKeyboard(chatID, text, sc.buttonText)
	}
	return p.deps.Sender.SendRemoveKeyboard(chatID, text)
}

// panelEarlyExit — шаг 3 сводной схемы ТЗ (EP-05): ранний выход панели,
// СТРОГО после Kanban.OnInbound и проверки режима M13. handled=true —
// inbound обслужен без Claude (welcome либо handoff), задача завершена.
//
// Ретраи: welcome сохраняется до Send — упавший Send ретраится веткой
// переотправки (последний в истории outbound); ветка кнопки идемпотентна
// через ранний выход по режиму (после handoff лид уже human — повтор гаснет
// на шаге 2, дубля уведомления нет).
func (p *Processor) panelEarlyExit(ctx context.Context, lead *models.Lead, inbound *models.Message, tgMsgID int, log *slog.Logger) (bool, error) {
	if p.deps.Panel == nil {
		return false, nil
	}
	sc := p.scenario(ctx)
	text := strings.TrimSpace(inbound.Content)

	// /start с непустым welcome → приветствие панели вместо Claude
	// (пустое welcome = поведение как до эпика, отвечает Эмма-LLM).
	if strings.HasPrefix(text, "/start") && sc.welcome != "" {
		tokens := estimateTokens(sc.welcome)
		author := models.AuthorBot
		out := &models.Message{LeadID: lead.ID, Author: &author, Content: sc.welcome, Tokens: &tokens}
		if err := p.deps.Msgs.CreateOutbound(ctx, out); err != nil {
			return true, fmt.Errorf("worker: сохранение welcome: %w", err)
		}
		p.publishMessage(ctx, out, lead.StageID, log)
		// Клавиатура — сразу с приветствия (решение владельца, ТЗ §10 п.2).
		if err := p.sendReply(ctx, lead.TelegramUserID, sc.welcome); err != nil {
			return true, fmt.Errorf("worker: отправка welcome: %w", err)
		}
		log.Info("worker: /start — отправлено приветствие панели, Claude не вызывался")
		return true, nil
	}

	// Нажатие кнопки менеджера — точное совпадение текста (TrimSpace).
	// Смена текста кнопки в панели старые клавиатуры не обновляет: старый
	// текст придёт сюда как обычный текст и уйдёт в Эмму — она распознает
	// просьбу маркером (мягкая деградация, ТЗ §3).
	if sc.buttonEnabled && text == sc.buttonText {
		return true, p.clientHandoff(ctx, lead, inbound, tgMsgID, sc.confirmText, log)
	}
	return false, nil
}

// clientHandoff — общая handoff-ветка EP-05 (ТЗ §4): клиент попросил
// менеджера кнопкой (confirmText — настроенное подтверждение) либо Эмма
// распознала просьбу маркером {{handoff}} (confirmText пуст: собственный
// ответ Эммы уже ушёл штатно, второй текст НЕ шлётся — ТЗ §4 п.2).
//
// Порядок шагов минимизирует ущерб при падении посередине: режим и контур
// напоминаний взводятся ДО подтверждения клиенту — упавший Send подтверждения
// теряет только текст, менеджер уже уведомлён и напоминание стоит.
func (p *Processor) clientHandoff(ctx context.Context, lead *models.Lead, inbound *models.Message, tgMsgID int, confirmText string, log *slog.Logger) error {
	d := p.deps

	// 1) Режим human механизмом M13: Эмма замолкает, менеджер ещё не взял
	// (taken_by NULL), пауза автопилота не смешивается с human.
	err := d.Leads.UpdateFields(ctx, lead.ID, map[string]interface{}{
		"dialog_mode":        models.DialogModeHuman,
		"bot_silenced_until": nil,
		"taken_by":           nil,
	})
	if errors.Is(err, repo.ErrNotFound) {
		log.Warn("worker: лид стёрт, handoff не нужен")
		return nil
	}
	if err != nil {
		return fmt.Errorf("worker: handoff: перевод в режим human: %w", err)
	}
	lead.DialogMode = models.DialogModeHuman
	lead.BotSilencedUntil = nil
	lead.TakenBy = nil

	// 2) WS: карточка в Kanban подсвечивается «требует ответа» (фронт M13
	// понимает dialog_mode; reason client_handoff — контракт EP-07).
	if d.Pub != nil {
		if err := d.Pub.Publish(ctx, events.DialogModeEvent(lead, events.ReasonClientHandoff)); err != nil {
			log.Warn("worker: событие dialog_mode не опубликовано", "error", err)
		}
	}

	// 3) Уведомление менеджерам в Telegram — канал M13 (уточнение ТЗ §4
	// п.4: ManagerChatID, НЕ alert_chat_id — тот для алертов EP-06).
	p.notifyManagerHandoff(lead, log)

	// 4) Немедленный взвод takeover:reminder по образцу M13 (тот же
	// TaskID-паттерн lead+message_count): менеджер молчит reminder_minutes →
	// напоминание, ещё pickup_minutes → Эмма подхватывает штатным контуром
	// (автоподхват 10+10 сохраняется и для client-handoff, ТЗ §10 п.7).
	if err := p.armHandoffReminder(ctx, lead, inbound, tgMsgID, log); err != nil {
		return err
	}

	// 5) Подтверждение клиенту — только для кнопки (Claude не вызывался).
	if confirmText != "" {
		tokens := estimateTokens(confirmText)
		author := models.AuthorBot
		out := &models.Message{LeadID: lead.ID, Author: &author, Content: confirmText, Tokens: &tokens}
		if err := d.Msgs.CreateOutbound(ctx, out); err != nil {
			return fmt.Errorf("worker: handoff: сохранение подтверждения: %w", err)
		}
		p.publishMessage(ctx, out, lead.StageID, log)
		if err := p.sendReply(ctx, lead.TelegramUserID, confirmText); err != nil {
			return fmt.Errorf("worker: handoff: отправка подтверждения: %w", err)
		}
	}

	// 6) След в статистике (вкладка 6 читается в EP-06). Best effort.
	p.recordEvent(ctx, &models.EmmaEvent{
		EventType: models.EmmaEventHandoff,
		LeadID:    &lead.ID,
	}, log)

	log.Info("worker: клиент передан менеджеру",
		"trigger", map[bool]string{true: "button", false: "marker"}[confirmText != ""])
	return nil
}

// notifyManagerHandoff — «Клиент <имя/username> просит менеджера (лид #N)»
// в чат менеджеров. Best effort: недоставленное уведомление не роняет
// handoff — напоминание reminder_minutes продублирует сигнал.
func (p *Processor) notifyManagerHandoff(lead *models.Lead, log *slog.Logger) {
	if p.deps.ManagerChatID == 0 {
		return // чат менеджеров не сконфигурирован — остаёмся при логе
	}
	text := fmt.Sprintf("🙋 CRM: клиент %s просит менеджера (лид #%d). Возьмите диалог в карточке.",
		leadDisplayName(lead), lead.ID)
	if err := p.deps.Sender.Send(p.deps.ManagerChatID, text); err != nil {
		log.Error("worker: уведомление о handoff не доставлено", "error", err)
	}
}

// leadDisplayName — имя лида для уведомления: name → @username → #id.
func leadDisplayName(l *models.Lead) string {
	switch {
	case l.Name != nil && strings.TrimSpace(*l.Name) != "":
		return strings.TrimSpace(*l.Name)
	case l.TgUsername != nil && *l.TgUsername != "":
		return "@" + *l.TgUsername
	default:
		return fmt.Sprintf("#%d", l.ID)
	}
}

// armHandoffReminder — взвод takeover:reminder сразу при handoff (EP-05):
// в отличие от armTakeoverReminder не проверяет «последнее сообщение —
// inbound» (подтверждение кнопки легально ложится outbound'ом следом);
// самогашение штатное — HandleReminder видит ответ менеджера и гаснет.
// Дедуп — TaskID (lead, message_count): повторная доставка не плодит задач.
func (p *Processor) armHandoffReminder(ctx context.Context, lead *models.Lead, inbound *models.Message, tgMsgID int, log *slog.Logger) error {
	d := p.deps
	if d.TakeoverEnq == nil || d.Settings == nil {
		return nil // юнит-тесты M3: контур напоминаний не собран
	}
	delay := time.Duration(d.Settings.Minutes(ctx, settings.KeyReminderMinutes)) * time.Minute
	err := d.TakeoverEnq.EnqueueTakeoverReminder(ctx, queue.TakeoverPayload{
		LeadID:       lead.ID,
		MessageCount: lead.MessageCount,
		InboundMsgID: inbound.ID,
		InboundAt:    inbound.CreatedAt,
		TgMsgID:      tgMsgID,
	}, delay)
	switch {
	case errors.Is(err, queue.ErrDuplicate):
		return nil // уже взведено (дубль апдейта) — §4.5 в работе
	case err != nil:
		return fmt.Errorf("worker: handoff: постановка напоминания: %w", err)
	}
	log.Info("worker: взведено напоминание менеджеру о handoff", "delay", delay.String())
	return nil
}

// contactsSection — секция контактов для system-блока (EP-05): активные
// контакты из провайдера (кэш 30 с). Без провайдера (юнит-тесты M3) и при
// пустом списке секции нет; боевой провайдер ошибок не возвращает
// (деградация внутри).
func (p *Processor) contactsSection(ctx context.Context, log *slog.Logger) string {
	if p.deps.Contacts == nil {
		return ""
	}
	contacts, err := p.deps.Contacts.Active(ctx)
	if err != nil {
		log.Warn("worker: список контактов для секции не получен, секция пропущена", "error", err)
		return ""
	}
	return contactsSection(contacts)
}

// promptConfig — активная конфигурация промпта. Без провайдера (юнит-тесты
// M3) и при его ошибке — fallback-константа: промпт не роняет диалог,
// боевой CachedPromptProvider ошибок и так не возвращает (fallback внутри).
func (p *Processor) promptConfig(ctx context.Context, log *slog.Logger) PromptConfig {
	if p.deps.Prompt == nil {
		return fallbackPromptConfig()
	}
	cfg, err := p.deps.Prompt.Current(ctx)
	if err != nil {
		log.Warn("worker: промпт из провайдера не получен, работаем на константе", "error", err)
		return fallbackPromptConfig()
	}
	return cfg
}

// filesSection — секция файлов для system-блока (EP-04): активные файлы из
// провайдера (кэш 30 с). Без провайдера (юнит-тесты M3) и при пустом списке
// секции нет; боевой провайдер ошибок не возвращает (деградация внутри).
func (p *Processor) filesSection(ctx context.Context, log *slog.Logger) string {
	if p.deps.FilesProv == nil {
		return ""
	}
	files, err := p.deps.FilesProv.Active(ctx)
	if err != nil {
		log.Warn("worker: список файлов для секции не получен, секция пропущена", "error", err)
		return ""
	}
	return sendFilesSection(files)
}

// sendMarkedFiles — отправка файлов по маркерам {{file:N}} (EP-04, ТЗ §3):
// валидация id по БД (мимо кэша — выключенный файл отсекается сразу),
// PDF → SendDocument, JPG/PNG → SendPhoto, по одному сообщению на файл
// (sendMediaGroup не используем). После успеха — служебная запись
// «[файл: <name>]» в messages (след в чате менеджера M12) + emma_events
// file_sent. Любая ошибка здесь НЕ роняет задачу: текст уже ушёл клиенту,
// ретрай продублировал бы ответ — только журнал и slog.
func (p *Processor) sendMarkedFiles(ctx context.Context, lead *models.Lead, fileIDs []int64, log *slog.Logger) {
	d := p.deps
	if len(fileIDs) == 0 {
		return
	}
	if d.SendFiles == nil {
		log.Warn("worker: маркеры файлов получены, но библиотека файлов не подключена",
			"file_ids", fileIDs)
		return
	}
	for _, id := range fileIDs {
		f, err := d.SendFiles.GetByID(ctx, id)
		switch {
		case errors.Is(err, repo.ErrNotFound):
			p.recordFileNotFound(ctx, lead.ID, id, "файл не найден", log)
			continue
		case err != nil:
			// БД мигнула между ответом и отправкой: ретраить нельзя (текст
			// ушёл) — файл пропускается, след остаётся в логе.
			log.Error("worker: файл маркера не прочитан из БД", "send_file_id", id, "error", err)
			continue
		}
		if !f.IsActive {
			p.recordFileNotFound(ctx, lead.ID, id, "файл выключен", log)
			continue
		}

		if err := p.sendFile(lead.TelegramUserID, f); err != nil {
			log.Error("worker: файл не отправлен в Telegram",
				"send_file_id", f.ID, "name", f.Name, "error", err)
			kind, detail := models.EmmaErrTelegramAPI, err.Error()
			p.recordEvent(ctx, &models.EmmaEvent{
				EventType:  models.EmmaEventError,
				ErrorKind:  &kind,
				Detail:     &detail,
				LeadID:     &lead.ID,
				SendFileID: &f.ID,
			}, log)
			continue
		}

		// След в чате менеджера (M12): служебная запись author=bot.
		// Файл уже у клиента — ошибка записи следа задачу не роняет.
		note := "[файл: " + f.Name + "]"
		noteTokens := estimateTokens(note)
		author := models.AuthorBot
		out := &models.Message{LeadID: lead.ID, Author: &author, Content: note, Tokens: &noteTokens}
		if err := d.Msgs.CreateOutbound(ctx, out); err != nil {
			log.Warn("worker: след отправки файла не сохранён",
				"send_file_id", f.ID, "error", err)
		} else {
			p.publishMessage(ctx, out, lead.StageID, log)
		}

		p.recordEvent(ctx, &models.EmmaEvent{
			EventType:  models.EmmaEventFileSent,
			LeadID:     &lead.ID,
			SendFileID: &f.ID,
		}, log)
		log.Info("worker: файл отправлен клиенту",
			"send_file_id", f.ID, "name", f.Name, "mime", f.MimeType)
	}
}

// sendFile — ветвление по mime (ТЗ §3): изображения фотографией, остальное
// (PDF) документом. Документу — человекочитаемое имя: на диске файл лежит
// под UUID, клиент должен увидеть название из панели.
func (p *Processor) sendFile(chatID int64, f *models.EmmaSendFile) error {
	if strings.HasPrefix(f.MimeType, "image/") {
		return p.deps.Sender.SendPhoto(chatID, f.FilePath)
	}
	fileName := f.Name
	if f.MimeType == "application/pdf" && !strings.HasSuffix(strings.ToLower(fileName), ".pdf") {
		fileName += ".pdf"
	}
	return p.deps.Sender.SendDocument(chatID, f.FilePath, fileName)
}

// recordFileNotFound — событие error/file_not_found: Эмма сослалась на
// несуществующий или выключенный файл (маркер уже вырезан, клиент ничего
// не заметил — след для вкладки 6 и алертов EP-06).
func (p *Processor) recordFileNotFound(ctx context.Context, leadID, fileID int64, why string, log *slog.Logger) {
	log.Error("worker: маркер ссылается на недоступный файл",
		"send_file_id", fileID, "reason", why)
	kind := models.EmmaErrFileNotFound
	detail := fmt.Sprintf("маркер {{file:%d}}: %s", fileID, why)
	p.recordEvent(ctx, &models.EmmaEvent{
		EventType: models.EmmaEventError,
		ErrorKind: &kind,
		Detail:    &detail,
		LeadID:    &leadID,
		// send_file_id НЕ пишем: файла либо нет (FK бы упал), либо он
		// выключен — для статистики «ошибка file_not_found» id живёт в detail.
	}, log)
}

// recordEvent — запись в emma_events (журнал вкладки 6) + сигнал контуру
// алертов EP-06 (единая точка: reply сбрасывает серии, error учитывается).
// Best effort: ни журнал, ни алерты не роняют диалог; nil-репозиторий —
// режим юнит-тестов M3; алерты не зависят от успеха записи журнала.
func (p *Processor) recordEvent(ctx context.Context, ev *models.EmmaEvent, log *slog.Logger) {
	if p.deps.Events != nil {
		if err := p.deps.Events.Create(ctx, ev); err != nil {
			log.Warn("worker: событие emma_events не записано",
				"event_type", ev.EventType, "error", err)
		}
	}
	if p.deps.Alerts == nil {
		return
	}
	switch {
	case ev.EventType == models.EmmaEventReply:
		p.deps.Alerts.OnSuccess(ctx)
	case ev.EventType == models.EmmaEventError && ev.ErrorKind != nil:
		detail := ""
		if ev.Detail != nil {
			detail = *ev.Detail
		}
		p.deps.Alerts.OnError(ctx, *ev.ErrorKind, detail)
	}
}

// errDetailLimit — потолок detail в emma_events: в журнал идёт краткий
// текст ошибки (у Claude-клиента там статус и message API — промпт клиент
// в ошибку не кладёт), простыни обрезаются.
const errDetailLimit = 300

// truncateDetail режет detail до errDetailLimit рун (кириллица — не байты).
func truncateDetail(s string) string {
	r := []rune(s)
	if len(r) <= errDetailLimit {
		return s
	}
	return string(r[:errDetailLimit]) + "…"
}

// errKindFor — различение таймаута и прикладной ошибки (task §1: по
// errors.Is): истёкший/отменённый контекст либо net.Error.Timeout
// (http.Client.Timeout Anthropic, дедлайны Telegram) → timeout, иначе
// fallback (llm_api для Claude, telegram_api для Send).
func errKindFor(err error, fallback string) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return models.EmmaErrTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return models.EmmaErrTimeout
	}
	return fallback
}

// recordClaudeError — событие error/llm_api|timeout после неудачного
// Complete (EP-06 §1). Пишется на КАЖДОЙ неудачной попытке: серия ретраев
// Asynq и должна двигать счётчик алертов.
func (p *Processor) recordClaudeError(ctx context.Context, leadID int64, cause error, log *slog.Logger) {
	kind := errKindFor(cause, models.EmmaErrLLMAPI)
	detail := truncateDetail(cause.Error())
	p.recordEvent(ctx, &models.EmmaEvent{
		EventType: models.EmmaEventError,
		ErrorKind: &kind,
		Detail:    &detail,
		LeadID:    &leadID,
	}, log)
}

// recordSendError — событие error/telegram_api|timeout после упавшего Send
// текста ответа (EP-06 §1): и первая попытка, и ветка переотправки.
func (p *Processor) recordSendError(ctx context.Context, leadID int64, cause error, log *slog.Logger) {
	kind := errKindFor(cause, models.EmmaErrTelegramAPI)
	detail := truncateDetail(cause.Error())
	p.recordEvent(ctx, &models.EmmaEvent{
		EventType: models.EmmaEventError,
		ErrorKind: &kind,
		Detail:    &detail,
		LeadID:    &leadID,
	}, log)
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
