package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/interfin/interfin-ai-crm/internal/claude"
	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/models"
)

// budgetConfig — боевые значения §7.2 / config.yaml.
func budgetConfig() config.ClaudeConfig {
	return config.ClaudeConfig{
		APIKey:            "k",
		Model:             "m",
		ClaudeReplyTokens: 1000,
		TokenBudget: config.TokenBudget{
			SystemPrompt: 2000,
			Summary:      1000,
			History:      4000,
			SafetyBuffer: 1000,
		},
		CountTokensThreshold: 7500,
	}
}

// fakeCounter — точный подсчёт в тестах: та же оценка len/4, что у бюджетера
// (детерминированно), плюс счётчик вызовов для критерия AQ²-7.
type fakeCounter struct {
	calls   int
	forbid  bool // true = вызов проваливает тест (оценка ниже порога)
	t       *testing.T
	results []int // если задано — выдаются по очереди вместо len/4
}

func (f *fakeCounter) CountTokens(_ context.Context, system string, msgs []claude.Message) (int, error) {
	f.calls++
	if f.forbid {
		f.t.Error("count_tokens вызван, хотя оценка ниже порога (AQ²-7)")
	}
	if len(f.results) > 0 {
		n := f.results[0]
		if len(f.results) > 1 {
			f.results = f.results[1:]
		}
		return n, nil
	}
	total := estimateTokens(system)
	for _, m := range msgs {
		total += estimateTokens(m.Content)
	}
	return total, nil
}

func newBudgeter(t *testing.T, counter Counter) *Budgeter {
	t.Helper()
	b, err := NewBudgeter(budgetConfig(), counter)
	if err != nil {
		t.Fatalf("NewBudgeter: %v", err)
	}
	return b
}

func inbound(content string) models.Message {
	return models.Message{Direction: models.DirectionInbound, Content: content}
}

func outbound(content string) models.Message {
	return models.Message{Direction: models.DirectionOutbound, Content: content}
}

// TestBuild_SmallDialog_NoCountTokens — AQ²-7: обычное сообщение обрабатывается
// БЕЗ похода в count_tokens.
func TestBuild_SmallDialog_NoCountTokens(t *testing.T) {
	counter := &fakeCounter{forbid: true, t: t}
	b := newBudgeter(t, counter)

	system, msgs, stats, err := b.Build(context.Background(), "Ты — ассистент.", "",
		[]models.Message{
			inbound("Здравствуйте, расскажите об услугах"),
			outbound("Здравствуйте! Мы предлагаем..."),
			inbound("А какие тарифы?"),
		})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if counter.calls != 0 || stats.ExactCalls != 0 {
		t.Errorf("count_tokens вызван %d раз, ожидали 0", counter.calls)
	}
	if system != "Ты — ассистент." {
		t.Errorf("system = %q", system)
	}
	want := []claude.Message{
		{Role: claude.RoleUser, Content: "Здравствуйте, расскажите об услугах"},
		{Role: claude.RoleAssistant, Content: "Здравствуйте! Мы предлагаем..."},
		{Role: claude.RoleUser, Content: "А какие тарифы?"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("msgs = %d, ожидали %d", len(msgs), len(want))
	}
	for i := range want {
		if msgs[i] != want[i] {
			t.Errorf("msgs[%d] = %+v, ожидали %+v", i, msgs[i], want[i])
		}
	}
}

// TestBuild_MessageMapping — склейка подряд идущих ролей, пропуск пустых,
// диалог начинается с user (требования Messages API).
func TestBuild_MessageMapping(t *testing.T) {
	b := newBudgeter(t, &fakeCounter{forbid: true, t: t})

	_, msgs, _, err := b.Build(context.Background(), "sys", "", []models.Message{
		outbound("ведущий assistant должен быть отброшен"),
		inbound("первое"),
		inbound("второе подряд"),
		outbound("ответ"),
		inbound(""), // стикер: пустое содержимое пропускается
		inbound("третье"),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := []claude.Message{
		{Role: claude.RoleUser, Content: "первое\nвторое подряд"},
		{Role: claude.RoleAssistant, Content: "ответ"},
		{Role: claude.RoleUser, Content: "третье"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("msgs = %+v, ожидали %+v", msgs, want)
	}
	for i := range want {
		if msgs[i] != want[i] {
			t.Errorf("msgs[%d] = %+v, ожидали %+v", i, msgs[i], want[i])
		}
	}
}

// TestBuild_HistoryWindow — история режется до budget.history (4000) оценкой,
// oldest-first; свежие сообщения выживают.
func TestBuild_HistoryWindow(t *testing.T) {
	b := newBudgeter(t, &fakeCounter{forbid: true, t: t})

	// 10 реплик по ~600 токенов (2400 байт) — все не влезут (лимит 4000).
	// Чередуем роли, чтобы склейка подряд идущих не слепила историю в одну.
	big := strings.Repeat("а", 2400) // кириллица: 2 байта/символ
	history := make([]models.Message, 0, 10)
	for i := 0; i < 9; i++ {
		if i%2 == 0 {
			history = append(history, inbound(big))
		} else {
			history = append(history, outbound(big))
		}
	}
	history = append(history, inbound("самое свежее"))

	_, msgs, stats, err := b.Build(context.Background(), "sys", "", history)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if stats.DroppedHistory == 0 {
		t.Error("окно обязано отбросить старые сообщения")
	}
	if got := estimateAll(msgs); got > 4000 {
		t.Errorf("история после окна: оценка %d > бюджета 4000", got)
	}
	lastContent := msgs[len(msgs)-1].Content
	if !strings.HasSuffix(lastContent, "самое свежее") {
		t.Errorf("свежайшее сообщение потеряно, хвост: %q", lastContent[len(lastContent)-40:])
	}
}

// TestBuild_ExactCountOnlyAtBoundary — AQ²-7: у границы бюджета count_tokens
// вызывается, и если точное значение в лимите — ровно один раз, без усечений.
// Границу с боевым бюджетом даёт сверхдлинное свежее сообщение: остальные
// компоненты жёстко капятся (2000+1000+4000 < 7500).
func TestBuild_ExactCountOnlyAtBoundary(t *testing.T) {
	counter := &fakeCounter{results: []int{7900}} // > порога 7500, но ≤ 8000
	b := newBudgeter(t, counter)

	system := strings.Repeat("s", 7600)  // ~1900 токенов
	summary := strings.Repeat("m", 3800) // ~950 токенов
	// Свежее сообщение ~5000 токенов: окно сохраняет его целиком (безусловно),
	// старшие реплики отбрасывает.
	history := []models.Message{
		inbound("старый вопрос"),
		outbound("старый ответ"),
		inbound(strings.Repeat("h", 5000*4)),
	}

	_, msgs, stats, err := b.Build(context.Background(), system, summary, history)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if stats.EstimateTokens <= 7500 {
		t.Fatalf("тест собран неверно: оценка %d не превысила порог", stats.EstimateTokens)
	}
	if counter.calls != 1 {
		t.Errorf("count_tokens вызван %d раз, ожидали ровно 1", counter.calls)
	}
	if stats.ExactTokens != 7900 {
		t.Errorf("ExactTokens = %d", stats.ExactTokens)
	}
	if len(msgs) != 1 {
		t.Errorf("контентных усечений быть не должно (точный подсчёт в лимите), msgs = %d", len(msgs))
	}
}

// TestBuild_TruncatesOldestFirst — точный подсчёт выше лимита → история
// усекается oldest-first до влезания (§7.2). Ветка активна при соотношениях
// threshold < бюджета (так будет при RAG в M4) — здесь порог занижен явно.
func TestBuild_TruncatesOldestFirst(t *testing.T) {
	cfg := budgetConfig()
	cfg.CountTokensThreshold = 500 // форсируем гибридную ветку на малой истории
	// Первый подсчёт 9500 (> 8000) → дроп старейшей пары; второй 8200 → ещё;
	// третий 7700 → влезли.
	counter := &fakeCounter{results: []int{9500, 8200, 7700}}
	b, err := NewBudgeter(cfg, counter)
	if err != nil {
		t.Fatalf("NewBudgeter: %v", err)
	}

	history := []models.Message{
		inbound(strings.Repeat("a", 800)),
		outbound(strings.Repeat("b", 800)),
		inbound(strings.Repeat("c", 800)),
		outbound(strings.Repeat("d", 800)),
		inbound("финальный вопрос " + strings.Repeat("e", 800)),
	}

	_, msgs, stats, err := b.Build(context.Background(), "sys", "", history)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if counter.calls != 3 {
		t.Errorf("count_tokens вызван %d раз, ожидали 3 (по одному на усечение)", counter.calls)
	}
	if stats.ExactTokens != 7700 {
		t.Errorf("итоговый точный подсчёт = %d, ожидали 7700", stats.ExactTokens)
	}
	// Каждое усечение снимает пару «старейший user + ставший ведущим
	// assistant» (нормализация): 5 → 3 → 1. Выживает свежий вопрос.
	if len(msgs) != 1 {
		t.Fatalf("после двух усечений ожидали 1 сообщение, got %d", len(msgs))
	}
	if stats.DroppedHistory != 4 {
		t.Errorf("DroppedHistory = %d, ожидали 4", stats.DroppedHistory)
	}
	if last := msgs[len(msgs)-1]; last.Role != claude.RoleUser ||
		!strings.HasPrefix(last.Content, "финальный вопрос") {
		t.Errorf("свежайшее сообщение потеряно: %+v", last)
	}
	if msgs[0].Role != claude.RoleUser {
		t.Errorf("после усечения диалог обязан начинаться с user, got %q", msgs[0].Role)
	}
}

// TestBuild_SingleOversizedMessage — safety buffer: единственное гигантское
// сообщение режется по содержимому, а не роняет задачу.
func TestBuild_SingleOversizedMessage(t *testing.T) {
	counter := &fakeCounter{results: []int{12000, 7500}}
	b := newBudgeter(t, counter)

	huge := strings.Repeat("оченьдлинное", 20000)
	_, msgs, _, err := b.Build(context.Background(), "sys", "",
		[]models.Message{inbound(huge)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("ожидали одно сообщение, got %d", len(msgs))
	}
	if len(msgs[0].Content) >= len(huge) {
		t.Error("содержимое сверхдлинного сообщения обязано быть усечено")
	}
	if counter.calls != 2 {
		t.Errorf("count_tokens вызван %d раз, ожидали 2 (до и после усечения)", counter.calls)
	}
}

// TestBuild_NeverExceedsBudget — IQ-6: на любом входе итоговый запрос
// (вход + claude_reply_tokens) ≤ 9000 токенов.
func TestBuild_NeverExceedsBudget(t *testing.T) {
	cfg := budgetConfig()
	inputLimit := cfg.TokenBudget.SystemPrompt + cfg.TokenBudget.Summary +
		cfg.TokenBudget.History + cfg.TokenBudget.SafetyBuffer // 8000
	if inputLimit+cfg.ClaudeReplyTokens != 9000 {
		t.Fatalf("конфиг разъехался с IQ-6: %d", inputLimit+cfg.ClaudeReplyTokens)
	}

	// «Честный» точный подсчёт = та же оценка len/4: проверяем инвариант
	// на выходе бюджетера при разных размерах входа.
	sizes := []int{10, 400, 4_000, 40_000, 120_000, 400_000}
	for _, size := range sizes {
		counter := &fakeCounter{}
		b := newBudgeter(t, counter)

		history := []models.Message{
			inbound(strings.Repeat("x", size)),
			outbound(strings.Repeat("y", size/2+1)),
			inbound(strings.Repeat("z", size) + " вопрос"),
		}
		system, msgs, _, err := b.Build(context.Background(),
			strings.Repeat("s", size), strings.Repeat("m", size), history)
		if err != nil {
			t.Fatalf("size %d: Build: %v", size, err)
		}

		total := estimateTokens(system) + estimateAll(msgs)
		if total > inputLimit {
			t.Errorf("size %d: вход %d токенов > лимита %d (IQ-6 нарушен)",
				size, total, inputLimit)
		}
		if total+cfg.ClaudeReplyTokens > 9000 {
			t.Errorf("size %d: запрос целиком %d > 9000 (IQ-6)", size, total+cfg.ClaudeReplyTokens)
		}
	}
}

func TestRetryDelay_Backoff(t *testing.T) {
	// SRS §6.3: 2/8/32 с; выход за границы упирается в крайние значения.
	for n, want := range map[int]string{0: "2s", 1: "2s", 2: "8s", 3: "32s", 10: "32s"} {
		if got := RetryDelay(n, nil, nil).String(); got != want {
			t.Errorf("RetryDelay(%d) = %s, ожидали %s", n, got, want)
		}
	}
}
