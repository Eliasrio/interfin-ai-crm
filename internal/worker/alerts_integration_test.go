// Интеграционный тест алертов EP-06 против реального Redis
// (REDIS_TEST_ADDR, иначе skip): фейк Claude падает сериями, настоящий
// emma.Notifier считает серии и анти-шум в Redis, фейковый Sender ловит
// алерты. Критерий приёмки: три llm_api подряд → РОВНО ОДИН алерт;
// четвёртая ошибка в 15-минутном окне — без второго; успешный ответ
// между ошибками сбрасывает серию.
package worker

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/interfin/interfin-ai-crm/internal/claude"
	"github.com/interfin/interfin-ai-crm/internal/emma"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/settings"
)

const alertsTestChatID = int64(-100987654321) // группа — отрицательный id

// alertsTestSettings — emma.Settings c одним ключом alert_chat_id.
type alertsTestSettings struct{}

func (alertsTestSettings) String(_ context.Context, key string) string {
	if key == settings.KeyAlertChatID {
		return "-100987654321"
	}
	return ""
}
func (alertsTestSettings) SetString(context.Context, string, string) error { return nil }

// alertsCatcher — emma.Sender: копит отправленные алерты.
type alertsCatcher struct {
	mu   sync.Mutex
	sent []sent
}

func (c *alertsCatcher) Send(chatID int64, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, sent{chatID: chatID, text: text})
	return nil
}

func (c *alertsCatcher) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

// alertsRedis — клиент тестового Redis + зачистка ключей emma:alert:*.
func alertsRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR не задан — интеграционный тест пропущен")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { rdb.Close() })
	purgeAlertKeys(t, rdb)
	t.Cleanup(func() { purgeAlertKeys(t, rdb) })
	return rdb
}

func purgeAlertKeys(t *testing.T, rdb redis.UniversalClient) {
	t.Helper()
	ctx := context.Background()
	iter := rdb.Scan(ctx, 0, "emma:alert:*", 100).Iterator()
	for iter.Next(ctx) {
		if err := rdb.Del(ctx, iter.Val()).Err(); err != nil {
			t.Fatalf("зачистка %s: %v", iter.Val(), err)
		}
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("scan emma:alert:*: %v", err)
	}
}

// alertsProcessor — процессор с настоящим Notifier поверх rdb.
func alertsProcessor(t *testing.T, ai *fakeAI, rdb redis.UniversalClient, catcher *alertsCatcher) (*Processor, *fakeMsgs) {
	t.Helper()
	notifier := emma.NewNotifier(rdb, alertsTestSettings{}, catcher, testLogger())
	msgs := &fakeMsgs{history: []models.Message{
		{LeadID: 7, Direction: models.DirectionInbound, Content: "Вопрос про инвестиции"},
	}}
	lead := *testLead
	p := NewProcessor(ProcessorDeps{
		Leads:    newFakeLeads(&lead),
		Msgs:     msgs,
		Budgeter: mustBudgeter(t),
		AI:       ai,
		Sender:   &fakeSender{},
		Events:   &fakeEvents{},
		Alerts:   notifier,
		Log:      testLogger(),
	})
	return p, msgs
}

// Критерий приёмки: 3 llm_api подряд → ровно ОДИН алерт в chat_id;
// четвёртая ошибка в течение 15 мин — без второго алерта.
func TestEP06_ThreeLLMErrorsOneAlert(t *testing.T) {
	rdb := alertsRedis(t)
	catcher := &alertsCatcher{}
	ai := &fakeAI{err: errors.New("claude: complete: api 500: internal error")}
	p, _ := alertsProcessor(t, ai, rdb, catcher)
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		if err := p.HandleProcessInbound(ctx, inboundTask(t, 7, 100+i)); err == nil {
			t.Fatalf("попытка %d: ждали ошибку Claude", i)
		}
	}
	if got := catcher.count(); got != 1 {
		t.Fatalf("после 3 ошибок подряд алертов %d, ждали ровно 1", got)
	}
	al := catcher.sent[0]
	if al.chatID != alertsTestChatID {
		t.Errorf("chat_id = %d, ждали %d (из settings)", al.chatID, alertsTestChatID)
	}
	// Формат ТЗ §3: тип проблемы, время, текст последней ошибки.
	if !strings.HasPrefix(al.text, "⚠️ Эмма: ") ||
		!strings.Contains(al.text, "Время: ") ||
		!strings.Contains(al.text, "Последняя ошибка: ") ||
		!strings.Contains(al.text, "api 500") {
		t.Errorf("формат алерта: %q", al.text)
	}

	// Четвёртая ошибка в окне анти-шума — второго алерта нет.
	if err := p.HandleProcessInbound(ctx, inboundTask(t, 7, 104)); err == nil {
		t.Fatal("ждали ошибку Claude")
	}
	if got := catcher.count(); got != 1 {
		t.Errorf("после 4-й ошибки алертов %d — анти-шум 15 минут нарушен", got)
	}
}

// Критерий приёмки: успешный ответ между ошибками сбрасывает серию —
// 2 ошибки, успех, 2 ошибки → алертов нет (порог 3 подряд не достигнут).
func TestEP06_SuccessResetsStreak(t *testing.T) {
	rdb := alertsRedis(t)
	catcher := &alertsCatcher{}
	ai := &fakeAI{err: errors.New("claude: complete: api 500: internal error")}
	p, msgs := alertsProcessor(t, ai, rdb, catcher)
	ctx := context.Background()

	// Две ошибки: серия = 2, порог не взят.
	for i := 1; i <= 2; i++ {
		if err := p.HandleProcessInbound(ctx, inboundTask(t, 7, 200+i)); err == nil {
			t.Fatal("ждали ошибку Claude")
		}
	}

	// Успешный ответ: fakeAI чинится, reply-событие сбрасывает серию.
	ai.mu.Lock()
	ai.err = nil
	ai.reply = "Всё работает"
	ai.usage = claude.Usage{InputTokens: 5, OutputTokens: 2}
	ai.mu.Unlock()
	if err := p.HandleProcessInbound(ctx, inboundTask(t, 7, 203)); err != nil {
		t.Fatalf("успешный ответ: %v", err)
	}

	// Новый inbound (после успеха последним лежит outbound — иначе ретрай
	// ушёл бы в ветку переотправки) и снова две ошибки.
	msgs.mu.Lock()
	msgs.history = append(msgs.history, models.Message{
		LeadID: 7, Direction: models.DirectionInbound, Content: "А подробнее?",
	})
	msgs.mu.Unlock()
	ai.mu.Lock()
	ai.err = errors.New("claude: complete: api 500: internal error")
	ai.mu.Unlock()
	for i := 1; i <= 2; i++ {
		if err := p.HandleProcessInbound(ctx, inboundTask(t, 7, 210+i)); err == nil {
			t.Fatal("ждали ошибку Claude")
		}
	}

	if got := catcher.count(); got != 0 {
		t.Errorf("алертов %d, ждали 0: успех обязан сбрасывать серию (2+2 < порога 3)", got)
	}
}
