// budget.go — токен-бюджетер контекста Claude (SRS §7.2, CLAUDE.md §4.6).
//
// Бюджет (config claude.token_budget): system 2000 + summary 1000 +
// history 4000 + buffer 1000 = 8000 входных токенов; ответ модели
// (claude_reply_tokens 1000) доводит потолок запроса до 9000 (IQ-6).
//
// Гибридный подсчёт (AQ²-fix #7): всегда локальная оценка len/4; точный
// count_tokens — ТОЛЬКО когда оценка превысила count_tokens_threshold (7500).
// Никакого tiktoken. Усечение истории — oldest-first.
package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/interfin/interfin-ai-crm/internal/claude"
	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/models"
)

// charsPerToken — грубая оценка SRS §7.2: estimate := len(text) / 4.
const charsPerToken = 4

// estimateTokens — локальная оценка без round-trip к Anthropic.
func estimateTokens(text string) int { return len(text) / charsPerToken }

// Counter — точный подсчёт токенов (в проде *claude.Client, в тестах фейк,
// который заодно считает число вызовов — критерий AQ²-7).
type Counter interface {
	CountTokens(ctx context.Context, system string, msgs []claude.Message) (int, error)
}

// BudgetStats — что бюджетер сделал с контекстом; уходит в структурный лог.
type BudgetStats struct {
	EstimateTokens int // локальная оценка итогового контекста
	ExactTokens    int // последний точный подсчёт (0 — count_tokens не вызывался)
	ExactCalls     int // сколько раз сходили в count_tokens (обычно 0)
	DroppedHistory int // сообщений истории отброшено (window + усечение)
}

// Budgeter собирает контекст запроса к Claude, не выходя за бюджет §7.2.
type Budgeter struct {
	budget    config.TokenBudget
	threshold int // count_tokens_threshold (7500)
	counter   Counter
}

func NewBudgeter(cfg config.ClaudeConfig, counter Counter) (*Budgeter, error) {
	b := cfg.TokenBudget
	if b.SystemPrompt <= 0 || b.Summary < 0 || b.History <= 0 || b.SafetyBuffer < 0 {
		return nil, fmt.Errorf("budgeter: невалидный token_budget: %+v", b)
	}
	if cfg.CountTokensThreshold <= 0 {
		return nil, errors.New("budgeter: count_tokens_threshold должен быть > 0")
	}
	if counter == nil {
		return nil, errors.New("budgeter: counter обязателен (гибридный подсчёт §4.6)")
	}
	return &Budgeter{budget: b, threshold: cfg.CountTokensThreshold, counter: counter}, nil
}

// inputLimit — потолок ВХОДНЫХ токенов запроса: сумма компонентов бюджета.
// Вместе с claude_reply_tokens это и есть общий лимит 9000 (IQ-6).
func (b *Budgeter) inputLimit() int {
	return b.budget.SystemPrompt + b.budget.Summary + b.budget.History + b.budget.SafetyBuffer
}

// Build собирает (system, messages) для Claude из системного промпта, сводки
// диалога (M3: пустая, появится в M4/§7.3) и истории сообщений (старые → новые).
//
// Гарантии:
//   - system ≤ budget.system_prompt, summary ≤ budget.summary (по оценке);
//   - история — sliding window ≤ budget.history (по оценке), oldest-first;
//   - если оценка всего запроса > threshold → точный count_tokens, и при
//     превышении inputLimit история усекается (oldest-first) до влезания.
func (b *Budgeter) Build(
	ctx context.Context,
	systemPrompt, summary string,
	history []models.Message,
) (string, []claude.Message, BudgetStats, error) {
	var stats BudgetStats

	system := truncateToTokens(systemPrompt, b.budget.SystemPrompt)
	if summary != "" {
		system += "\n\nСводка предыдущего диалога:\n" + truncateToTokens(summary, b.budget.Summary)
	}

	msgs := toClaudeMessages(history)
	converted := len(msgs)

	// Sliding window по оценке: идём от новых к старым, пока влезает.
	// Свежайшее сообщение сохраняется ВСЕГДА, даже сверхдлинное: на него
	// рассчитан safety buffer (§7.2), а его перерост дальше поймает гибридный
	// точный подсчёт. Иначе оценка окна не превышала бы порог никогда.
	kept := 0
	used := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		cost := estimateTokens(msgs[i].Content)
		if kept > 0 && used+cost > b.budget.History {
			break
		}
		used += cost
		kept++
	}
	msgs = normalizeLeadingRole(msgs[len(msgs)-kept:])
	stats.DroppedHistory = converted - len(msgs)
	if len(msgs) == 0 {
		return "", nil, stats, errors.New(
			"budgeter: в истории нет пригодных сообщений для контекста")
	}

	// Гибрид §4.6: почти всегда хватает оценки — без похода в count_tokens.
	stats.EstimateTokens = estimateTokens(system) + estimateAll(msgs)
	if stats.EstimateTokens <= b.threshold {
		return system, msgs, stats, nil
	}

	// Оценка у границы — только теперь точный подсчёт (AQ²-7).
	limit := b.inputLimit()
	contentTruncated := false
	for {
		exact, err := b.counter.CountTokens(ctx, system, msgs)
		if err != nil {
			return "", nil, stats, fmt.Errorf("budgeter: count_tokens: %w", err)
		}
		stats.ExactCalls++
		stats.ExactTokens = exact
		if exact <= limit {
			return system, msgs, stats, nil
		}

		// Усечение oldest-first (§7.2). С боевым бюджетом сюда попадает
		// обычно единственное сверхдлинное сообщение; ветка с дропом старших
		// работает при иных соотношениях threshold/бюджета (например, когда
		// M4 добавит RAG-chunks в system).
		if len(msgs) > 1 {
			before := len(msgs)
			msgs = normalizeLeadingRole(msgs[1:])
			stats.DroppedHistory += before - len(msgs)
			continue
		}
		if contentTruncated {
			return "", nil, stats, fmt.Errorf(
				"budgeter: %d токенов после усечения — не влезает в лимит %d", exact, limit)
		}
		// Осталось единственное (текущее) сообщение, и оно само сверх лимита —
		// на него и рассчитан safety buffer: жёстко режем содержимое.
		room := limit - estimateTokens(system)
		if room <= 0 {
			return "", nil, stats, fmt.Errorf(
				"budgeter: системный промпт съел весь лимит %d", limit)
		}
		msgs[0].Content = tailRunes(msgs[0].Content, room*charsPerToken)
		contentTruncated = true
	}
}

func estimateAll(msgs []claude.Message) int {
	total := 0
	for _, m := range msgs {
		total += estimateTokens(m.Content)
	}
	return total
}

// toClaudeMessages — messages §8.2 → формат Messages API:
// inbound → user, outbound → assistant; пустые (стикеры и т.п.) пропускаются;
// подряд идущие реплики одной роли склеиваются (API требует чередования).
func toClaudeMessages(history []models.Message) []claude.Message {
	msgs := make([]claude.Message, 0, len(history))
	for _, m := range history {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		role := claude.RoleUser
		if m.Direction == models.DirectionOutbound {
			role = claude.RoleAssistant
		}
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Content += "\n" + content
			continue
		}
		msgs = append(msgs, claude.Message{Role: role, Content: content})
	}
	return msgs
}

// normalizeLeadingRole отбрасывает ведущие assistant-реплики: после усечения
// oldest-first диалог обязан начинаться с user (требование Messages API).
func normalizeLeadingRole(msgs []claude.Message) []claude.Message {
	for len(msgs) > 0 && msgs[0].Role != claude.RoleUser {
		msgs = msgs[1:]
	}
	return msgs
}

// truncateToTokens режет строку до maxTokens по оценке len/4,
// не разрывая UTF-8 (важно: диалоги русскоязычные).
func truncateToTokens(s string, maxTokens int) string {
	if estimateTokens(s) <= maxTokens {
		return s
	}
	return headRunes(s, maxTokens*charsPerToken)
}

// headRunes — первые maxBytes байт строки с выравниванием по границе руны.
func headRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	runes := []rune(s)
	out := make([]rune, 0, len(runes))
	size := 0
	for _, r := range runes {
		size += len(string(r))
		if size > maxBytes {
			break
		}
		out = append(out, r)
	}
	return string(out)
}

// tailRunes — последние maxBytes байт строки с выравниванием по границе руны
// (для истории ценнее свежий хвост сообщения, чем его начало).
func tailRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	runes := []rune(s)
	size := 0
	start := len(runes)
	for i := len(runes) - 1; i >= 0; i-- {
		size += len(string(runes[i]))
		if size > maxBytes {
			break
		}
		start = i
	}
	return string(runes[start:])
}
