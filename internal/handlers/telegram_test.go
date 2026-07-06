package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

const testSecret = "test-secret-token"

// updateJSON — минимальный валидный апдейт Telegram с текстовым сообщением.
func updateJSON(tgUserID int64, msgID int, text string) string {
	return fmt.Sprintf(`{
		"update_id": 10,
		"message": {
			"message_id": %d,
			"from": {"id": %d, "first_name": "Иван", "last_name": "Петров", "username": "ivan"},
			"chat": {"id": %d, "type": "private"},
			"text": %q
		}
	}`, msgID, tgUserID, tgUserID, text)
}

// --- фейки зависимостей ---

type fakeLeads struct {
	byTgID    map[int64]*models.Lead
	nextID    int64
	created   []*models.Lead
	updates   map[int64]map[string]interface{}
	createErr error
	getErr    error
	updateErr error
}

func newFakeLeads() *fakeLeads {
	return &fakeLeads{
		byTgID:  map[int64]*models.Lead{},
		nextID:  100,
		updates: map[int64]map[string]interface{}{},
	}
}

func (f *fakeLeads) Create(_ context.Context, lead *models.Lead) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.nextID++
	lead.ID = f.nextID
	f.byTgID[lead.TelegramUserID] = lead
	f.created = append(f.created, lead)
	return nil
}

func (f *fakeLeads) GetByID(_ context.Context, id int64) (*models.Lead, error) {
	for _, l := range f.byTgID {
		if l.ID == id {
			return l, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (f *fakeLeads) GetByTelegramUserID(_ context.Context, tgUserID int64) (*models.Lead, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if l, ok := f.byTgID[tgUserID]; ok {
		return l, nil
	}
	return nil, fmt.Errorf("lead: %w", repo.ErrNotFound)
}

func (f *fakeLeads) Save(_ context.Context, _ *models.Lead) error { return nil }

func (f *fakeLeads) UpdateFields(_ context.Context, id int64, fields map[string]interface{}) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updates[id] = fields
	return nil
}

func (f *fakeLeads) TransitionStage(_ context.Context, _ int64, _, _ int16) (bool, error) {
	panic("webhook не двигает стадии — это делает state machine в воркере (M5)")
}

func (f *fakeLeads) List(context.Context, repo.ListLeadsParams) ([]models.Lead, int64, error) {
	panic("webhook не листает лидов (метод M8; фейк для /api — в api_contract_test.go)")
}

type fakeMsgs struct {
	inbound    []*models.Message
	inboundErr error
}

func (f *fakeMsgs) CreateInbound(_ context.Context, msg *models.Message) error {
	if f.inboundErr != nil {
		return f.inboundErr
	}
	msg.ID = int64(len(f.inbound) + 1)
	msg.Direction = models.DirectionInbound
	f.inbound = append(f.inbound, msg)
	return nil
}

func (f *fakeMsgs) CreateOutbound(_ context.Context, msg *models.Message) error {
	panic("webhook не имеет права создавать outbound (CLAUDE.md §4.3)")
}

func (f *fakeMsgs) ListByLead(_ context.Context, _ int64, _ int) ([]models.Message, error) {
	return nil, nil
}

type enqueueCall struct {
	LeadID int64
	MsgID  int
}

type fakeQueue struct {
	calls []enqueueCall
	err   error
}

func (f *fakeQueue) EnqueueInbound(_ context.Context, leadID int64, msgID int) error {
	f.calls = append(f.calls, enqueueCall{LeadID: leadID, MsgID: msgID})
	return f.err
}

// dispatchRecorder — заглушка финального звена (в prod — telegram.Dispatcher):
// проверяет, что тело запроса дошло восстановленным.
type dispatchRecorder struct {
	called bool
	body   []byte
}

func (d *dispatchRecorder) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	d.called = true
	d.body, _ = io.ReadAll(r.Body)
}

type env struct {
	router   *gin.Engine
	leads    *fakeLeads
	msgs     *fakeMsgs
	queue    *fakeQueue
	dispatch *dispatchRecorder
}

func newEnv(t *testing.T) *env {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := &env{
		leads:    newFakeLeads(),
		msgs:     &fakeMsgs{},
		queue:    &fakeQueue{},
		dispatch: &dispatchRecorder{},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.router = gin.New()
	NewTelegramWebhook(e.leads, e.msgs, e.queue, testSecret, log).
		Register(e.router, e.dispatch)
	return e
}

func (e *env) post(body string, secret string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhook/telegram", bytes.NewBufferString(body))
	if secret != "" {
		req.Header.Set(SecretTokenHeader, secret)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// --- §5.4: Secret Token ---

func TestSecretToken_Missing403(t *testing.T) {
	e := newEnv(t)
	w := e.post(updateJSON(777, 1, "hi"), "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("без секрета ожидали 403, получили %d", w.Code)
	}
	if len(e.msgs.inbound) != 0 || len(e.queue.calls) != 0 {
		t.Error("запрос без секрета не должен доходить до сохранения/очереди")
	}
}

func TestSecretToken_Invalid403(t *testing.T) {
	e := newEnv(t)
	w := e.post(updateJSON(777, 1, "hi"), "wrong-secret")
	if w.Code != http.StatusForbidden {
		t.Fatalf("с неверным секретом ожидали 403, получили %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("ERR_WEBHOOK_FORBIDDEN")) {
		t.Errorf("ошибка должна быть в формате {error, code}: %s", w.Body.String())
	}
}

func TestSecretToken_EmptyConfiguredSecretRejectsAll(t *testing.T) {
	// Пустой секрет на сервере — это misconfiguration, а не «пускать всех».
	gin.SetMode(gin.TestMode)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := gin.New()
	NewTelegramWebhook(newFakeLeads(), &fakeMsgs{}, &fakeQueue{}, "", log).
		Register(router, &dispatchRecorder{})

	req := httptest.NewRequest(http.MethodPost, "/webhook/telegram", bytes.NewBufferString(updateJSON(1, 1, "x")))
	req.Header.Set(SecretTokenHeader, "")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("пустой секрет в конфиге должен закрывать webhook: ожидали 403, получили %d", w.Code)
	}
}

// --- ingestion: сохранить → 200 → очередь ---

func TestSaveAndReturn200_NewLead(t *testing.T) {
	e := newEnv(t)
	w := e.post(updateJSON(777, 1001, "Привет, хочу открыть счёт"), testSecret)

	if w.Code != http.StatusOK {
		t.Fatalf("ожидали 200, получили %d (%s)", w.Code, w.Body.String())
	}

	// Лид создан по telegram_user_id, в первой стадии Kanban.
	if len(e.leads.created) != 1 {
		t.Fatalf("ожидали создание одного лида, создано %d", len(e.leads.created))
	}
	lead := e.leads.created[0]
	if lead.TelegramUserID != 777 || lead.StageID != 1 {
		t.Errorf("лид создан неверно: tg=%d stage=%d", lead.TelegramUserID, lead.StageID)
	}
	if lead.Name == nil || *lead.Name != "Иван Петров" {
		t.Errorf("имя лида не заполнено из апдейта: %v", lead.Name)
	}

	// Сообщение сохранено как inbound (контракт: до ответа 200).
	if len(e.msgs.inbound) != 1 {
		t.Fatalf("ожидали одно inbound-сообщение, сохранено %d", len(e.msgs.inbound))
	}
	msg := e.msgs.inbound[0]
	if msg.LeadID != lead.ID || msg.Content != "Привет, хочу открыть счёт" {
		t.Errorf("сообщение сохранено неверно: %+v", msg)
	}
	if msg.Direction != models.DirectionInbound {
		t.Errorf("direction = %q, ожидали inbound", msg.Direction)
	}

	// Задача поставлена с telegram message_id (основа дедуп-ключа).
	if len(e.queue.calls) != 1 {
		t.Fatalf("ожидали один enqueue, получили %d", len(e.queue.calls))
	}
	if e.queue.calls[0] != (enqueueCall{LeadID: lead.ID, MsgID: 1001}) {
		t.Errorf("enqueue с неверными аргументами: %+v", e.queue.calls[0])
	}

	// Тело дошло до финального звена (telebot) восстановленным.
	if !e.dispatch.called || len(e.dispatch.body) == 0 {
		t.Error("апдейт должен дойти до telebot-диспетчера с восстановленным телом")
	}
}

func TestSaveAndReturn200_ExistingLeadNotRecreated(t *testing.T) {
	e := newEnv(t)
	e.post(updateJSON(777, 1, "первое"), testSecret)
	e.post(updateJSON(777, 2, "второе"), testSecret)

	if len(e.leads.created) != 1 {
		t.Errorf("лид должен создаваться один раз, создано %d", len(e.leads.created))
	}
	if len(e.msgs.inbound) != 2 {
		t.Errorf("оба сообщения должны сохраниться, сохранено %d", len(e.msgs.inbound))
	}
}

func TestSaveAndReturn200_DuplicateUpdateStill200(t *testing.T) {
	e := newEnv(t)
	e.queue.err = queue.ErrDuplicate

	w := e.post(updateJSON(777, 1001, "hi"), testSecret)
	if w.Code != http.StatusOK {
		t.Fatalf("дубль апдейта — не ошибка: ожидали 200, получили %d", w.Code)
	}
	if len(e.leads.updates) != 0 {
		t.Error("дубль не должен трогать pending_task")
	}
}

func TestSaveAndReturn200_RedisDownSetsPendingTask(t *testing.T) {
	// Graceful degradation (SRS §11.2): сообщение уже в БД, Redis лёг —
	// лида помечаем pending_task=TRUE и всё равно отвечаем 200.
	e := newEnv(t)
	e.queue.err = errors.New("redis: connection refused")

	w := e.post(updateJSON(777, 1001, "hi"), testSecret)
	if w.Code != http.StatusOK {
		t.Fatalf("Redis down не должен ронять webhook: ожидали 200, получили %d", w.Code)
	}
	lead := e.leads.created[0]
	fields, ok := e.leads.updates[lead.ID]
	if !ok || fields["pending_task"] != true {
		t.Errorf("при недоступном Redis лид должен получить pending_task=true, получили %v", e.leads.updates)
	}
}

func TestSaveAndReturn200_SaveFailedNo200(t *testing.T) {
	// Контракт M2: сообщение сохранено ДО 200. Не сохранили — отдаём 5xx,
	// Telegram перепошлёт апдейт.
	e := newEnv(t)
	e.msgs.inboundErr = errors.New("db down")

	w := e.post(updateJSON(777, 1001, "hi"), testSecret)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("при несохранённом сообщении ожидали 500, получили %d", w.Code)
	}
	if len(e.queue.calls) != 0 {
		t.Error("несохранённое сообщение нельзя ставить в очередь")
	}
}

func TestSaveAndReturn200_NonMessageUpdateSkipped(t *testing.T) {
	e := newEnv(t)
	w := e.post(`{"update_id": 11, "edited_message": {"message_id": 5, "from": {"id": 777}, "text": "edited"}}`, testSecret)

	if w.Code != http.StatusOK {
		t.Fatalf("не-сообщение должно получать 200, получили %d", w.Code)
	}
	if len(e.msgs.inbound) != 0 || len(e.queue.calls) != 0 {
		t.Error("edited_message не сохраняется и не ставится в очередь в M2")
	}
}

func TestSaveAndReturn200_GarbageBody200(t *testing.T) {
	e := newEnv(t)
	w := e.post(`{not json`, testSecret)
	if w.Code != http.StatusOK {
		t.Fatalf("нечитаемый апдейт: ожидали 200 (не зацикливать ретраи), получили %d", w.Code)
	}
	if e.dispatch.called {
		t.Error("мусорное тело не должно доходить до telebot")
	}
}

func TestSaveAndReturn200_MediaCaptionSaved(t *testing.T) {
	e := newEnv(t)
	body := `{"update_id":12,"message":{"message_id":7,"from":{"id":42,"first_name":"A"},"chat":{"id":42,"type":"private"},"caption":"фото договора"}}`
	w := e.post(body, testSecret)
	if w.Code != http.StatusOK {
		t.Fatalf("ожидали 200, получили %d", w.Code)
	}
	if len(e.msgs.inbound) != 1 || e.msgs.inbound[0].Content != "фото договора" {
		t.Errorf("caption должен сохраняться как content: %+v", e.msgs.inbound)
	}
}

// --- IQ-1: 200 быстрее 300 мс ---

func TestWebhook_RespondsUnder300ms(t *testing.T) {
	// В хендлере нет сетевых вызовов (Claude и т.п.) — только БД и enqueue.
	// С фейками фиксируем бюджет самого кода цепочки: он обязан быть
	// пренебрежимо мал относительно лимита 300 мс (IQ-1).
	e := newEnv(t)
	start := time.Now()
	w := e.post(updateJSON(777, 1001, "ping"), testSecret)
	elapsed := time.Since(start)

	if w.Code != http.StatusOK {
		t.Fatalf("ожидали 200, получили %d", w.Code)
	}
	if elapsed > 300*time.Millisecond {
		t.Errorf("webhook ответил за %v — дольше лимита 300 мс (IQ-1)", elapsed)
	}
}
