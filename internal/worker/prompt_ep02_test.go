// Тесты EP-02: промпт из БД (провайдер с кэшем 30 с, fallback на константу),
// сборка system-блока секциями (темы/стиль/язык, порядок ТЗ §3) и бюджет
// system-блока 5000 (решение владельца 2026-07-11).
package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// --- фейк EmmaPromptRepo ---

type fakePromptRepo struct {
	mu      sync.Mutex
	current *models.EmmaPromptVersion // nil — таблица пуста (ErrNotFound)
	err     error                     // если задана — GetCurrent падает (БД лежит)
	calls   int
}

func (f *fakePromptRepo) GetCurrent(context.Context) (*models.EmmaPromptVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.current == nil {
		return nil, repo.ErrNotFound
	}
	cp := *f.current
	return &cp, nil
}

func (f *fakePromptRepo) set(v *models.EmmaPromptVersion, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current, f.err = v, err
}

func (f *fakePromptRepo) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakePromptRepo) CreateVersion(context.Context, *models.EmmaPromptVersion) error {
	panic("воркер версии не создаёт (ручки EP-02)")
}

func (f *fakePromptRepo) History(context.Context, int, int) ([]repo.EmmaPromptHistoryItem, int64, error) {
	panic("воркер историю не листает (ручки EP-02)")
}

func (f *fakePromptRepo) GetByID(context.Context, int64) (*models.EmmaPromptVersion, error) {
	panic("воркер по id не читает (ручки EP-02)")
}

// dbVersion — версия «из БД» для тестов провайдера.
func dbVersion(text string, topics string, style string) *models.EmmaPromptVersion {
	return &models.EmmaPromptVersion{
		ID: 1, SystemPrompt: text,
		ForbiddenTopics: models.JSONB(topics),
		Style:           style, IsCurrent: true,
	}
}

// --- PromptProvider: fallback и кэш ---

// TestPromptProvider_EmptyTableFallsBackToConstant — критерий приёмки:
// пустая таблица (свежая БД без 0021) → воркер работает на константе.
func TestPromptProvider_EmptyTableFallsBackToConstant(t *testing.T) {
	p := NewPromptProvider(&fakePromptRepo{}, testLogger())
	cfg, err := p.Current(context.Background())
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if cfg.Text != systemPrompt {
		t.Error("пустая таблица обязана давать константу systemPrompt")
	}
	if cfg.Style != models.EmmaStyleNeutral || len(cfg.ForbiddenTopics) != 0 {
		t.Errorf("fallback: style=%q topics=%v, ждали neutral и пусто", cfg.Style, cfg.ForbiddenTopics)
	}
}

// TestPromptProvider_ReadsVersionAndParsesTopics — версия из БД доезжает
// целиком: текст, разобранный JSONB тем, стиль.
func TestPromptProvider_ReadsVersionAndParsesTopics(t *testing.T) {
	rp := &fakePromptRepo{current: dbVersion("Новый промпт.", `["политика","крипто-советы"]`, models.EmmaStyleFormal)}
	cfg, err := NewPromptProvider(rp, testLogger()).Current(context.Background())
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if cfg.Text != "Новый промпт." || cfg.Style != models.EmmaStyleFormal {
		t.Errorf("config: %+v", cfg)
	}
	if len(cfg.ForbiddenTopics) != 2 || cfg.ForbiddenTopics[0] != "политика" {
		t.Errorf("topics: %v", cfg.ForbiddenTopics)
	}
}

// TestPromptProvider_BrokenTopicsDegradeToEmpty — битый JSONB (ручная правка
// в БД) не роняет диалог: темы пустые, текст живой.
func TestPromptProvider_BrokenTopicsDegradeToEmpty(t *testing.T) {
	rp := &fakePromptRepo{current: dbVersion("Текст.", `не json`, models.EmmaStyleNeutral)}
	cfg, err := NewPromptProvider(rp, testLogger()).Current(context.Background())
	if err != nil || cfg.Text != "Текст." || len(cfg.ForbiddenTopics) != 0 {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

// TestPromptProvider_CacheTTL — дисциплина кэша 30 с (паттерн settings):
// в пределах TTL БД не дёргается и отдаётся старое значение, после тика —
// свежее из БД.
func TestPromptProvider_CacheTTL(t *testing.T) {
	rp := &fakePromptRepo{current: dbVersion("v1", `[]`, models.EmmaStyleNeutral)}
	p := NewPromptProvider(rp, testLogger())
	base := time.Now()
	now := base
	p.now = func() time.Time { return now }
	ctx := context.Background()

	if cfg, _ := p.Current(ctx); cfg.Text != "v1" {
		t.Fatalf("первое чтение: %q", cfg.Text)
	}

	// Правка «из панели»: до истечения TTL воркер живёт на кэше.
	rp.set(dbVersion("v2", `[]`, models.EmmaStyleNeutral), nil)
	now = base.Add(29 * time.Second)
	if cfg, _ := p.Current(ctx); cfg.Text != "v1" {
		t.Errorf("внутри TTL: %q, ждали кэшированный v1", cfg.Text)
	}
	if rp.callCount() != 1 {
		t.Errorf("БД дёрнута %d раз, ждали 1 (кэш)", rp.callCount())
	}

	// Тик кэша — свежее значение (правка доезжает ≤30 с, ТЗ §3).
	now = base.Add(31 * time.Second)
	if cfg, _ := p.Current(ctx); cfg.Text != "v2" {
		t.Errorf("после TTL: %q, ждали v2", cfg.Text)
	}
}

// TestPromptProvider_DBErrorKeepsLastKnown — ошибка БД деградирует до
// последнего удачного значения (не до константы) и не кэшируется.
func TestPromptProvider_DBErrorKeepsLastKnown(t *testing.T) {
	rp := &fakePromptRepo{current: dbVersion("боевой", `[]`, models.EmmaStyleNeutral)}
	p := NewPromptProvider(rp, testLogger())
	base := time.Now()
	now := base
	p.now = func() time.Time { return now }
	ctx := context.Background()

	if cfg, _ := p.Current(ctx); cfg.Text != "боевой" {
		t.Fatalf("первое чтение: %q", cfg.Text)
	}
	rp.set(nil, errors.New("pg: connection refused"))
	now = base.Add(31 * time.Second)
	if cfg, err := p.Current(ctx); err != nil || cfg.Text != "боевой" {
		t.Errorf("при ошибке БД: %q err=%v, ждали последний удачный без ошибки", cfg.Text, err)
	}

	// БД ожила — следующее чтение видит свежее (ошибка не закэширована).
	rp.set(dbVersion("после починки", `[]`, models.EmmaStyleNeutral), nil)
	if cfg, _ := p.Current(ctx); cfg.Text != "после починки" {
		t.Errorf("после починки БД: %q", cfg.Text)
	}
}

// TestPromptProvider_DBErrorWithoutCacheFallsBack — БД лежит с первого
// чтения (кэша ещё нет) → константа, диалог не падает.
func TestPromptProvider_DBErrorWithoutCacheFallsBack(t *testing.T) {
	rp := &fakePromptRepo{err: errors.New("pg down")}
	cfg, err := NewPromptProvider(rp, testLogger()).Current(context.Background())
	if err != nil || cfg.Text != systemPrompt {
		t.Fatalf("cfg.Text != константа при мёртвой БД без кэша (err=%v)", err)
	}
}

// --- сборка system-блока (unit prompt.go, критерий приёмки) ---

// TestBuildSystemBase_SectionsAndOrder — непустые темы и не-neutral стиль
// дают секции, порядок фиксирован ТЗ §3: текст → темы → стиль → язык.
func TestBuildSystemBase_SectionsAndOrder(t *testing.T) {
	cfg := PromptConfig{
		Text:            "База промпта.",
		ForbiddenTopics: []string{"политика", "религия"},
		Style:           models.EmmaStyleFormal,
	}
	got := buildSystemBase(cfg, nil)

	topics := "Никогда не обсуждай следующие темы: политика, религия. " +
		"Вежливо возвращай разговор к услугам сервиса."
	style := styleSections[models.EmmaStyleFormal]
	lang := languageInstruction(nil)
	for name, part := range map[string]string{
		"текст из БД": cfg.Text, "запретные темы": topics, "стиль": style, "язык (M14)": lang,
	} {
		if !strings.Contains(got, part) {
			t.Errorf("в system-блоке нет секции «%s»:\n%s", name, got)
		}
	}
	if !(strings.Index(got, cfg.Text) < strings.Index(got, topics) &&
		strings.Index(got, topics) < strings.Index(got, style) &&
		strings.Index(got, style) < strings.Index(got, lang)) {
		t.Errorf("порядок секций нарушен (ТЗ §3):\n%s", got)
	}
}

// TestBuildSystemBase_EmptyTopicsNeutralStyle — критерий приёмки: пустой
// список тем не добавляет секцию, neutral не добавляет текста — system-блок
// в точности «текст + языковая инструкция».
func TestBuildSystemBase_EmptyTopicsNeutralStyle(t *testing.T) {
	cfg := PromptConfig{Text: "База промпта.", Style: models.EmmaStyleNeutral}
	got := buildSystemBase(cfg, nil)
	want := "База промпта.\n\n" + languageInstruction(nil)
	if got != want {
		t.Errorf("neutral/пустые темы обязаны давать только текст+язык:\n got: %q\nwant: %q", got, want)
	}
	if strings.Contains(got, "Никогда не обсуждай") {
		t.Error("секция запретных тем появилась при пустом списке")
	}
}

// TestBuildSystemBase_LanguageFollowsLead — языковая инструкция M14 осталась
// кодом и следует за языком лида (не редактируется из панели).
func TestBuildSystemBase_LanguageFollowsLead(t *testing.T) {
	en := "en"
	got := buildSystemBase(fallbackPromptConfig(), &en)
	if !strings.Contains(got, languageInstruction(&en)) {
		t.Error("языковая инструкция не соответствует языку лида")
	}
}

// --- бюджет 5000 (задача 5, критерий приёмки) ---

// TestBuild_SystemPrompt5000_HistoryNotEaten — system-блок 4900 токенов
// (законный после EP-02) + история: оценка превышает threshold 7500 → идёт
// точный count_tokens, но запрос собирается, история НЕ съедена, вход в
// пределах inputLimit 11000.
func TestBuild_SystemPrompt5000_HistoryNotEaten(t *testing.T) {
	cfg := budgetConfig()
	cfg.TokenBudget.SystemPrompt = 5000 // config.yaml после EP-02
	counter := &fakeCounter{}           // точный подсчёт разрешён: len/4
	b, err := NewBudgeter(cfg, counter)
	if err != nil {
		t.Fatalf("NewBudgeter: %v", err)
	}

	system := strings.Repeat("s", 4900*charsPerToken) // 4900 токенов
	history := []models.Message{}
	for i := 0; i < 30; i++ { // 30 × 100 токенов = 3000 (внутри history 4000);
		// роли чередуются — toClaudeMessages не склеит их в одно сообщение
		msg := inbound(strings.Repeat("m", 100*charsPerToken))
		if i%2 == 1 {
			msg = outbound(strings.Repeat("m", 100*charsPerToken))
		}
		history = append(history, msg)
	}

	gotSystem, msgs, stats, err := b.Build(context.Background(), system, "", history)
	if err != nil {
		t.Fatalf("Build не должен ломаться при system > threshold: %v", err)
	}
	if gotSystem != system {
		t.Error("system 4900 токенов усечён, хотя влезает в бюджет 5000")
	}
	if stats.DroppedHistory != 0 || len(msgs) != len(history) {
		t.Errorf("история съедена: dropped=%d, msgs=%d/%d",
			stats.DroppedHistory, len(msgs), len(history))
	}
	if stats.ExactCalls == 0 {
		t.Error("оценка 7900 > threshold 7500 — обязан был сходить в count_tokens")
	}
	if limit := b.inputLimit(); limit != 11000 || stats.ExactTokens > limit {
		t.Errorf("inputLimit=%d, exact=%d — потолок входа 11000 нарушен", limit, stats.ExactTokens)
	}
}

// --- contract-тест процессора (критерий приёмки: без рестарта, ≤30 с) ---

// TestProcessor_PromptEditReachesClaudeWithinTTL — правка промпта через
// панель попадает в system-блок следующего запроса к Claude без рестарта
// воркера: до тика кэша Эмма живёт на старом тексте, после тика (≤30 с) —
// на новом, включая секцию запретных тем.
func TestProcessor_PromptEditReachesClaudeWithinTTL(t *testing.T) {
	rp := &fakePromptRepo{current: dbVersion(
		"Ты — Эмма, версия до правки.", `[]`, models.EmmaStyleNeutral)}
	provider := NewPromptProvider(rp, testLogger())
	base := time.Now()
	now := base
	provider.now = func() time.Time { return now }

	leads := newFakeLeads(&models.Lead{ID: 7, TelegramUserID: 424242, StageID: 2})
	ai := &fakeAI{reply: "ответ Эммы"}
	newProc := func(history ...models.Message) (*Processor, *fakeMsgs) {
		msgs := &fakeMsgs{history: history}
		return NewProcessor(ProcessorDeps{
			Leads:    leads,
			Msgs:     msgs,
			Budgeter: mustBudgeter(t),
			AI:       ai,
			Sender:   &fakeSender{},
			Prompt:   provider,
			Log:      testLogger(),
		}), msgs
	}

	// 1) Сообщение до правки — system со старым текстом.
	p, _ := newProc(models.Message{LeadID: 7, Direction: models.DirectionInbound, Content: "Здравствуйте!"})
	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle 1: %v", err)
	}
	if !strings.Contains(ai.lastSystem(), "версия до правки") {
		t.Fatalf("system без текста из БД:\n%s", ai.lastSystem())
	}

	// 2) Правка из панели (новая активная версия в БД) + сообщение ВНУТРИ
	// TTL — воркер ещё на кэше (это ок: контракт «максимум 30 с»).
	rp.set(dbVersion("Ты — Эмма, версия после правки.",
		`["политика"]`, models.EmmaStyleNeutral), nil)
	now = base.Add(15 * time.Second)
	p, _ = newProc(models.Message{LeadID: 7, Direction: models.DirectionInbound, Content: "Вопрос?"})
	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 101)); err != nil {
		t.Fatalf("handle 2: %v", err)
	}
	if !strings.Contains(ai.lastSystem(), "версия до правки") {
		t.Error("внутри TTL воркер обязан жить на кэшированной версии")
	}

	// 3) Тик кэша — следующий запрос к Claude уже с новым текстом и темами.
	now = base.Add(31 * time.Second)
	p, _ = newProc(models.Message{LeadID: 7, Direction: models.DirectionInbound, Content: "Ещё вопрос?"})
	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 102)); err != nil {
		t.Fatalf("handle 3: %v", err)
	}
	sys := ai.lastSystem()
	if !strings.Contains(sys, "версия после правки") {
		t.Errorf("после тика кэша system без нового текста:\n%s", sys)
	}
	if !strings.Contains(sys, "Никогда не обсуждай следующие темы: политика.") {
		t.Errorf("после тика кэша system без секции запретных тем:\n%s", sys)
	}
	if strings.Contains(sys, "версия до правки") {
		t.Error("старый текст промпта остался в system-блоке")
	}
}
