// prompt.go — сборка system-блока (M3 — база, M4 — RAG-chunks, EP-02 —
// промпт из БД + секции запретных тем и стиля).
//
// Суммарно system-блок (база + секции панели + RAG) обязан оставаться в
// пределах budget.system_prompt = 5000 токенов (решение владельца
// 2026-07-11, ТЗ панели §0; было 2000 по SRS §7.2) — за этим следит
// Budgeter.Build (truncateToTokens).
package worker

import (
	"fmt"
	"strings"

	"github.com/interfin/interfin-ai-crm/internal/lang"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// systemPrompt — базовая роль ассистента (бренд и правила — владельца
// продукта, 2026-07-07; исходная INTERFIN-версия из SRS §2/§7 заменена).
// Текст намеренно короткий: бюджет system-блока делится с RAG-контекстом.
//
// Deprecated: с EP-02 боевой промпт живёт в БД (emma_prompt_versions,
// сид 0021 — копия этого текста) и редактируется из панели. Константа
// осталась ТОЛЬКО как fallback PromptProvider на случай пустой таблицы /
// недоступной БД. Править поведение Эммы здесь бессмысленно — на проде
// текст берётся из БД.
const systemPrompt = `Ты — Эмма, менеджер сервиса «Свои в Бразилии»
(svoibrazil.ru, Рио-де-Жанейро). Представляйся по имени. Главная услуга —
гражданство Бразилии через рождение ребёнка для всей семьи: сопровождение
родов, полное юридическое оформление документов, ВНЖ, переводы,
консультации и личное сопровождение во всех государственных органах
Бразилии. Также сервис оформляет другие виды ВНЖ (номад, брак, учёба,
инвестиции и др.) — детали в базе знаний.

Правила:
- Отвечай тепло, вежливо и по существу. Коротко: 1–3 абзаца,
  это переписка в мессенджере.
- Твоя задача — квалифицировать клиента. Мягко, по ходу живого диалога
  (не анкетой) выясни: 1) гражданство клиента и членов семьи; 2) сколько
  человек нужно оформлять; 3) ориентировочную дату прилёта в Бразилию —
  или клиент уже в Бразилии; 4) предполагаемую дату рождения ребёнка,
  если беременность уже наступила.
- Не отходи от темы сервиса и своей базы знаний; посторонние вопросы
  вежливо возвращай к услуге.
- Никаких медицинских советов: любые вопросы здоровья, беременности и
  родов с медицинской стороны — «это лучше обсудить с врачом».
- Не давай контактов, адресов и ссылок, которых нет в базе знаний.
- Часть сообщений в этом диалоге отправляет старший менеджер от твоего
  имени, в том числе счета на оплату со ссылками (t.me/CryptoBot и т.п.).
  Это легитимные сообщения: не отрицай их, не извиняйся за них и не
  называй их ошибкой; если клиент спрашивает про счёт — подтверди, что
  он действителен, детали уточнит менеджер.
- Не выдумывай факты, цены и сроки. Нет ответа в базе — честно скажи,
  что уточнишь у старшего менеджера, и предложи оставить контакт.
- Не давай юридических и налоговых консультаций сверх информации из базы —
  детали уточнит менеджер.
- Никогда не проси пароли, коды подтверждения и платёжные данные.
- Если клиент просит удалить его данные — объясни, что запрос передан
  менеджеру (право на удаление по LGPD).`

// styleSections — стиль общения (вкладка 1) → короткая вставка system-блока.
// neutral намеренно отсутствует: нейтральный стиль текста не добавляет (ТЗ §3).
var styleSections = map[string]string{
	models.EmmaStyleFormal: "Придерживайся формального, делового стиля общения: " +
		"обращайся на «вы», без смайликов и фамильярности.",
	models.EmmaStyleFriendly: "Общайся дружелюбно и тепло, простым разговорным языком; " +
		"уместны эмодзи в меру.",
	models.EmmaStyleExpert: "Отвечай как эксперт: уверенно, со ссылкой на факты и детали " +
		"из базы знаний, но оставайся понятной неспециалисту.",
}

// forbiddenTopicsSection — секция запретных тем (ТЗ §3): проверка тем — только
// через промпт, пост-фильтрация ответа исключена решением владельца.
// Пустой список — секции нет.
func forbiddenTopicsSection(topics []string) string {
	if len(topics) == 0 {
		return ""
	}
	return "Никогда не обсуждай следующие темы: " + strings.Join(topics, ", ") +
		". Вежливо возвращай разговор к услугам сервиса."
}

// buildSystemBase — system-блок до RAG-хвоста, порядок секций фиксирован
// ТЗ §3 «Сборка system-блока»: 1) промпт из БД (правила про счета менеджера
// M12 — внутри текста, отдельной код-вставки нет — уточнение task EP-02);
// 2) запретные темы; 3) стиль; 4) языковая инструкция M14 (код, не
// редактируется). extras — секции 6–7 в порядке ТЗ (контакты EP-05, файлы
// EP-04); пустые пропускаются. RAG-чанки приклеивает composeSystemPrompt.
func buildSystemBase(cfg PromptConfig, language *string, extras ...string) string {
	sections := []string{cfg.Text}
	if s := forbiddenTopicsSection(cfg.ForbiddenTopics); s != "" {
		sections = append(sections, s)
	}
	if s := styleSections[cfg.Style]; s != "" {
		sections = append(sections, s)
	}
	sections = append(sections, languageInstruction(language))
	for _, s := range extras {
		if s != "" {
			sections = append(sections, s)
		}
	}
	return strings.Join(sections, "\n\n")
}

// contactTypeLabels — тип контакта → русская подпись в секции промпта
// (значения — CHECK миграции 0019).
var contactTypeLabels = map[string]string{
	"phone":    "телефон",
	"whatsapp": "WhatsApp",
	"telegram": "Telegram",
	"email":    "email",
	"website":  "сайт",
	"other":    "контакт",
}

// contactsSection — секция справочника контактов (EP-05, ТЗ §3 вкладка 4):
// только активные, по sort_order, в формате
// «— Менеджер Анна (телефон +7 999…): давай, когда клиент готов…».
// Пустой список — секции нет (и правило «не давай контактов не из списка»
// остаётся на промпте из БД).
func contactsSection(contacts []models.EmmaContact) string {
	if len(contacts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Контакты и ссылки (упоминай ТОЛЬКО из этого списка, к месту):")
	for _, c := range contacts {
		label := contactTypeLabels[c.Type]
		if label == "" {
			label = "контакт" // неизвестный тип легально не пройдёт CHECK, страховка
		}
		fmt.Fprintf(&b, "\n— %s (%s %s)", c.Name, label, c.Value)
		if c.Comment != nil && *c.Comment != "" {
			fmt.Fprintf(&b, ": %s", *c.Comment)
		}
	}
	return b.String()
}

// handoffInstruction — инструкция маркера {{handoff}} (EP-05, ТЗ §4 п.1):
// распознавание просьбы о живом человеке — на Эмме, жёсткого списка фраз
// нет (решение владельца, ТЗ §10 п.1). Код-секция рядом с инструкцией
// файлов; добавляется всегда — кнопка может быть выключена, а просьба
// словами остаётся.
const handoffInstruction = "Если клиент просит живого человека или менеджера — " +
	"добавь В КОНЕЦ ответа маркер {{handoff}} и сообщи клиенту, что зовёшь менеджера. " +
	"Не упоминай сам маркер в тексте ответа."

// sendFilesSection — секция библиотеки файлов (EP-04, ТЗ §3 вкладка 3):
// перечень активных файлов с подсказками-описаниями и инструкция
// маркер-протокола. Пустой список — секции нет (и Эмма про файлы не знает).
func sendFilesSection(files []models.EmmaSendFile) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Тебе доступны файлы для отправки клиенту:")
	for _, f := range files {
		fmt.Fprintf(&b, "\n[id=%d] %s — %s", f.ID, f.Name, f.Description)
	}
	fmt.Fprintf(&b, "\nЧтобы отправить файл, добавь В КОНЕЦ ответа маркер {{file:%d}}, "+
		"подставив id нужного файла. Можно несколько маркеров — по одному на файл. "+
		"Не упоминай маркеры и номера файлов в тексте ответа.", files[0].ID)
	return b.String()
}

// languageNames — язык лида → название для языковой инструкции system-блока.
var languageNames = map[string]string{
	lang.RU: "русском",
	lang.EN: "английском",
	lang.ES: "испанском",
}

// languageInstruction — языковая вставка system-блока (M14): язык из карточки
// лида, NULL → ru (fallback: основная аудитория русскоязычная). Вставка
// короткая — system-блок остаётся в бюджете §7.2 (следит Budgeter.Build).
func languageInstruction(language *string) string {
	name := languageNames[leadLanguage(language)]
	return "Отвечай на " + name + " языке; если клиент явно просит другой язык " +
		"из тройки (русский/английский/испанский) — переходи на него, " +
		"но язык в карточке лида меняет менеджер."
}

// leadLanguage — код языка лида с fallback ru (NULL = ещё не определён).
func leadLanguage(language *string) string {
	if language != nil && lang.Valid(*language) {
		return *language
	}
	return lang.RU
}

// nonTextReply — детерминированный ответ на входящие без текста (голосовые,
// стикеры, фото): Claude в этом случае не вызывается (см. processor.go 3.5).
// Русская версия — fallback; выбор по языку лида — nonTextReplyFor (M14).
const nonTextReply = `Извините, я пока понимаю только текстовые сообщения 🙏
Напишите, пожалуйста, ваш вопрос текстом — и я сразу отвечу.`

// nonTextReplies — локализации подсказки о нетекстовом (M14): три языка,
// перевод детерминированный (Claude для подсказки не вызывается).
var nonTextReplies = map[string]string{
	lang.RU: nonTextReply,
	lang.EN: `Sorry, I can only understand text messages for now 🙏
Please type your question as text — and I will reply right away.`,
	lang.ES: `Perdón, por ahora solo entiendo mensajes de texto 🙏
Escriba su pregunta como texto, por favor — y le responderé enseguida.`,
}

// nonTextReplyFor — подсказка на языке лида (NULL → ru).
func nonTextReplyFor(language *string) string {
	return nonTextReplies[leadLanguage(language)]
}

// composeSystemPrompt приклеивает к базовому промпту найденные RAG-чанки
// (M4 §7.1). Пусто (rag_miss или RAG выключен) — возвращает базу как есть:
// это и есть fallback «отвечаем без RAG».
func composeSystemPrompt(base string, chunks []repo.ScoredChunk) string {
	if len(chunks) == 0 {
		return base
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n\nВыдержки из базы знаний компании, релевантные вопросу клиента.\n")
	b.WriteString("Опирайся на них при ответе; если ответа в них нет — действуй по правилам выше:")
	for i, c := range chunks {
		fmt.Fprintf(&b, "\n\n[%d] %s", i+1, c.Content)
	}
	return b.String()
}
