// Тесты критериев приёмки M9:
//   - stage_change через Redis → доставка клиенту < 500 мс (IQ-10);
//   - reconnect после expiry → catch-up updated_since (AQ²-5);
//   - единственный heartbeat — protocol-level; idle-клиент закрывается
//     по read deadline (AQ²-11);
//   - истёкший JWT при upgrade → close 4001 (§5.3);
//   - обрыв pub/sub → сигнал polling_mode, восстановление → live_mode (§10.1).
//
// Тесты с Redis (доставка, реконнект) требуют REDIS_TEST_ADDR — как
// интеграционные тесты queue/worker; без него skip.
package ws

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/handlers"
	"github.com/interfin/interfin-ai-crm/internal/kanban"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// --- инфраструктура ---

// testKeyVal — общая RSA-пара тестов пакета (генерация на каждый тест —
// секунды впустую, как в auth/jwt_test.go).
var (
	testKeyOnce sync.Once
	testKeyVal  *rsa.PrivateKey
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("rsa: %v", err)
		}
		testKeyVal = key
	})
	return testKeyVal
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// wsLeads — минимальный repo.LeadRepo для catch-up-теста: только List
// (GET /api/leads) и точечное обновление активности.
type wsLeads struct {
	mu    sync.Mutex
	leads map[int64]*models.Lead
}

func newWSLeads(leads ...*models.Lead) *wsLeads {
	f := &wsLeads{leads: map[int64]*models.Lead{}}
	for _, l := range leads {
		f.leads[l.ID] = l
	}
	return f
}

func (f *wsLeads) Create(context.Context, *models.Lead) error { panic("не зовётся") }
func (f *wsLeads) GetByTelegramUserID(context.Context, int64) (*models.Lead, error) {
	panic("не зовётся")
}
func (f *wsLeads) Save(context.Context, *models.Lead) error { panic("не зовётся") }
func (f *wsLeads) UpdateFields(context.Context, int64, map[string]interface{}) error {
	return nil // state machine чистит ttl_task_id — для WS-теста несущественно
}

func (f *wsLeads) GetByID(_ context.Context, id int64) (*models.Lead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lead, ok := f.leads[id]
	if !ok {
		return nil, repo.ErrNotFound
	}
	cp := *lead
	return &cp, nil
}

// TransitionStage — CAS как в боевом repo (стадия, сброс anti-spam,
// last_activity_at, CLAUDE.md §4.7).
func (f *wsLeads) TransitionStage(_ context.Context, id int64, from, to int16) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lead, ok := f.leads[id]
	if !ok || lead.StageID != from {
		return false, nil
	}
	lead.StageID = to
	lead.AntiSpamCount = 0
	lead.LastActivityAt = time.Now().UTC()
	return true, nil
}

// Остальные зависимости kanban.Machine, безразличные WS-пути: доставку
// события решают Publisher (боевой RedisPublisher) и Hub, а TTL/anti-spam/
// сообщения — контур M5, закрытый его же тестами.

type wsMsgs struct{}

func (wsMsgs) CreateInbound(context.Context, *models.Message) error  { panic("не зовётся") }
func (wsMsgs) CreateOutbound(context.Context, *models.Message) error { return nil }
func (wsMsgs) ListByLead(context.Context, int64, int) ([]models.Message, error) {
	return nil, nil
}

func (wsMsgs) ListByLeadBefore(context.Context, int64, int64, int) ([]models.Message, error) {
	return nil, nil
}

func (wsMsgs) HasManagerOutboundAfter(context.Context, int64, int64) (bool, error) {
	return false, nil // контур takeover (M13) в WS-тестах не участвует
}

type noopTTL struct{}

func (noopTTL) Schedule(context.Context, int64, time.Duration) error { return nil }
func (noopTTL) Cancel(context.Context, int64) error                  { return nil }

type noopAntiSpam struct{}

func (noopAntiSpam) Schedule(context.Context, int64, int16, time.Duration, time.Duration) (bool, error) {
	return false, nil
}
func (noopAntiSpam) Cancel(context.Context, int64) error { return nil }

type noopSender struct{}

func (noopSender) Send(int64, string) error { return nil }

func (f *wsLeads) List(_ context.Context, p repo.ListLeadsParams) ([]models.Lead, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []models.Lead
	for _, l := range f.leads {
		// Тот же строгий After, что в боевом repo (§10.3).
		if p.UpdatedSince != nil && !l.LastActivityAt.After(*p.UpdatedSince) {
			continue
		}
		out = append(out, *l)
	}
	return out, int64(len(out)), nil
}

func (f *wsLeads) touch(id int64, at time.Time, stage int16) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leads[id].LastActivityAt = at
	f.leads[id].StageID = stage
}

// rig — сервер как в cmd/server: /ws/kanban + (для catch-up) /api/leads
// за auth-цепочкой M7.
type rig struct {
	hub   *Hub
	srv   *httptest.Server
	leads *wsLeads
}

func newRig(t *testing.T, timings Timings) *rig {
	t.Helper()
	gin.SetMode(gin.TestMode)
	key := testKey(t)
	verifier := auth.NewVerifier(&key.PublicKey)
	hub := NewHub(timings, testLogger())

	r := gin.New()
	NewHandler(hub, verifier, testLogger()).Register(r)

	leads := newWSLeads()
	api := r.Group("/api")
	api.Use(
		auth.Middleware(verifier),
		auth.RequireRole(auth.RoleManager, auth.RoleAdmin),
	)
	handlers.NewLeads(handlers.LeadsDeps{Leads: leads, Log: testLogger()}).Register(api)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &rig{hub: hub, srv: srv, leads: leads}
}

// token выпускает access-токен с нужным сроком жизни (просрочка — ttl < 0).
func (r *rig) token(t *testing.T, ttl time.Duration, role string) string {
	t.Helper()
	token, err := auth.NewIssuer(testKey(t), ttl).Issue("42", role)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return token
}

// dial подключается к /ws/kanban с JWT в Sec-WebSocket-Protocol (§5.3).
func (r *rig) dial(token string) (*websocket.Conn, *http.Response, error) {
	url := "ws" + strings.TrimPrefix(r.srv.URL, "http") + "/ws/kanban"
	d := websocket.Dialer{Subprotocols: []string{"Bearer." + token}}
	return d.Dial(url, nil)
}

// waitClients ждёт регистрации n клиентов в Hub'е (handshake у Dial
// завершается раньше, чем serve() успевает вызвать register).
func (r *rig) waitClients(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.hub.mu.Lock()
		got := len(r.hub.clients)
		r.hub.mu.Unlock()
		if got == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("клиентов в hub не стало %d", n)
}

// reader — фоновая вычитка соединения: text-кадры в канал, ошибка — в err.
type reader struct {
	msgs chan []byte
	err  chan error
}

func readLoop(conn *websocket.Conn) *reader {
	r := &reader{msgs: make(chan []byte, 64), err: make(chan error, 1)}
	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				r.err <- err
				return
			}
			r.msgs <- msg
		}
	}()
	return r
}

func (r *reader) next(t *testing.T, within time.Duration) []byte {
	t.Helper()
	select {
	case msg := <-r.msgs:
		return msg
	case err := <-r.err:
		t.Fatalf("соединение оборвано вместо сообщения: %v", err)
	case <-time.After(within):
		t.Fatalf("сообщение не пришло за %v", within)
	}
	return nil
}

func testRedisAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR не задан — интеграционный WS-тест пропущен")
	}
	return addr
}

// runHub запускает hub.Run на живом Redis и дожидается активной подписки
// (прогревочными publish'ами — Receive-подтверждение видно только Hub'у).
func runHub(t *testing.T, r *rig, addr string) (redis.UniversalClient, *events.RedisPublisher) {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { rdb.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.hub.Run(ctx, rdb)
	return rdb, events.NewRedisPublisher(rdb)
}

// warmup гоняет прогревочные события, пока клиент не получит первое:
// после этого и подписка Hub'а, и клиент точно живы.
func warmup(t *testing.T, pub *events.RedisPublisher, rd *reader) events.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ev := events.Event{Type: events.TypeStageChange, LeadID: -1, StageID: 1}
		if err := pub.Publish(context.Background(), ev); err != nil {
			t.Fatalf("warmup publish: %v", err)
		}
		select {
		case msg := <-rd.msgs:
			var got events.Event
			if err := json.Unmarshal(msg, &got); err != nil {
				t.Fatalf("warmup unmarshal: %v (%s)", err, msg)
			}
			return got
		case err := <-rd.err:
			t.Fatalf("warmup: соединение оборвано: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("warmup: событие так и не дошло")
	return events.Event{}
}

// --- критерий 4: истёкший JWT при upgrade → close 4001 (§5.3) ---

func TestUpgrade_ExpiredToken_Close4001(t *testing.T) {
	r := newRig(t, DefaultTimings)
	conn, _, err := r.dial(r.token(t, -time.Minute, auth.RoleManager))
	if err != nil {
		t.Fatalf("handshake обязан пройти (иначе браузер не увидит 4001): %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = conn.ReadMessage()
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != CloseTokenExpired {
		t.Fatalf("ожидали close %d, получили: %v", CloseTokenExpired, err)
	}
}

// Битый/отсутствующий токен — не reconnect-сценарий: обычный 401 без upgrade.
func TestUpgrade_InvalidToken_401(t *testing.T) {
	r := newRig(t, DefaultTimings)
	conn, resp, err := r.dial("мусор")
	if err == nil {
		conn.Close()
		t.Fatal("handshake с битым токеном не должен пройти")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ожидали 401, получили %+v", resp)
	}
}

// §5.2: system — только internal, WS-доска ему не положена.
func TestUpgrade_SystemRole_403(t *testing.T) {
	r := newRig(t, DefaultTimings)
	conn, resp, err := r.dial(r.token(t, time.Minute, "system"))
	if err == nil {
		conn.Close()
		t.Fatal("handshake для роли system не должен пройти")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ожидали 403, получили %+v", resp)
	}
}

// --- критерий 3 (AQ²-11): один heartbeat-механизм, idle закрывается ---

// Живой клиент: сервер шлёт ТОЛЬКО protocol-level ping (никаких app-level
// {type:"ping"} в data-кадрах), pong держит соединение живым много циклов.
func TestHeartbeat_ProtocolLevelOnly(t *testing.T) {
	timings := Timings{
		PingInterval: 40 * time.Millisecond,
		PongWait:     120 * time.Millisecond,
		WriteWait:    time.Second,
	}
	r := newRig(t, timings)
	conn, _, err := r.dial(r.token(t, time.Hour, auth.RoleManager))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	var pings int32
	stdPing := conn.PingHandler() // дефолт gorilla отвечает pong'ом
	conn.SetPingHandler(func(data string) error {
		atomic.AddInt32(&pings, 1)
		return stdPing(data)
	})
	rd := readLoop(conn)

	// 10 ping-интервалов: при любом дубляже heartbeat'а app-level ping
	// пришёл бы data-кадром в rd.msgs.
	wait := time.After(10 * timings.PingInterval)
	for {
		select {
		case msg := <-rd.msgs:
			t.Fatalf("сервер прислал data-кадр вне событий (app-level heartbeat?): %s", msg)
		case err := <-rd.err:
			t.Fatalf("соединение с pong'ами не должно рваться: %v", err)
		case <-wait:
			if got := atomic.LoadInt32(&pings); got < 2 {
				t.Fatalf("protocol ping'ов: %d, ожидали >= 2", got)
			}
			return
		}
	}
}

// Idle-клиент (без pong'ов) закрывается сервером по read deadline —
// единственный и достаточный механизм обнаружения мёртвого соединения.
func TestHeartbeat_IdleConnectionCloses(t *testing.T) {
	timings := Timings{
		PingInterval: 40 * time.Millisecond,
		PongWait:     120 * time.Millisecond,
		WriteWait:    time.Second,
	}
	r := newRig(t, timings)
	conn, _, err := r.dial(r.token(t, time.Hour, auth.RoleManager))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetPingHandler(func(string) error { return nil }) // молчим: pong не уходит
	rd := readLoop(conn)

	start := time.Now()
	select {
	case err := <-rd.err:
		alive := time.Since(start)
		if alive > 10*timings.PongWait {
			t.Fatalf("idle-соединение жило %v — deadline не работает", alive)
		}
		_ = err // разрыв без close-кадра (сервер умер по deadline) — норма
	case msg := <-rd.msgs:
		t.Fatalf("неожиданный data-кадр: %s", msg)
	case <-time.After(10 * timings.PongWait):
		t.Fatal("idle-соединение не закрыто сервером")
	}
	r.waitClients(t, 0) // и из Hub'а клиент снят
}

// --- §10.1: обрыв pub/sub → polling_mode, восстановление → live_mode ---

func TestPubSubDegraded_Signals(t *testing.T) {
	r := newRig(t, DefaultTimings)
	conn, _, err := r.dial(r.token(t, time.Hour, auth.RoleManager))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	r.waitClients(t, 1)
	rd := readLoop(conn)

	// setDegraded — ровно то, что зовёт pump при ошибке ReceiveMessage.
	r.hub.setDegraded(true)
	var sig struct {
		Type     string `json:"type"`
		Interval int    `json:"poll_interval_sec"`
	}
	if err := json.Unmarshal(rd.next(t, 2*time.Second), &sig); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sig.Type != "polling_mode" || sig.Interval != 5 {
		t.Fatalf("ожидали polling_mode/5с, получили %+v", sig)
	}

	// Повторная деградация состояния не спамит (ретраи подписки молчат).
	r.hub.setDegraded(true)
	r.hub.setDegraded(false)
	if err := json.Unmarshal(rd.next(t, 2*time.Second), &sig); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sig.Type != "live_mode" {
		t.Fatalf("ожидали live_mode, получили %+v", sig)
	}

	// Клиент, подключившийся ВО ВРЕМЯ деградации, узнаёт о ней сразу.
	r.hub.setDegraded(true)
	rd.next(t, 2*time.Second) // polling_mode первому клиенту
	conn2, _, err := r.dial(r.token(t, time.Hour, auth.RoleManager))
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer conn2.Close()
	rd2 := readLoop(conn2)
	if err := json.Unmarshal(rd2.next(t, 2*time.Second), &sig); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sig.Type != "polling_mode" {
		t.Fatalf("новый клиент должен получить polling_mode, получил %+v", sig)
	}
}

// --- критерий 1 (IQ-10): смена стадии в воркере → клиент < 500 мс ---

// Полное плечо доставки: kanban.Machine.Transition (боевая state machine
// M5 + боевой events.RedisPublisher) → Redis pub/sub → Hub → WS-клиент.
// Фейковый только слой БД (CAS-семантика повторена) — его латентность
// покрыта запасом критерия: pgx-транзакция CAS на локальном Postgres
// на порядки меньше 500 мс.
func TestStageChange_DeliveredUnder500ms(t *testing.T) {
	addr := testRedisAddr(t)
	r := newRig(t, DefaultTimings)
	rdb, pub := runHub(t, r, addr)

	r.leads.leads[42] = &models.Lead{
		ID: 42, TelegramUserID: 4200, StageID: 2,
		LastActivityAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}
	machine := kanban.NewMachine(r.leads, wsMsgs{}, noopTTL{}, noopAntiSpam{},
		events.NewRedisPublisher(rdb), noopSender{},
		config.KanbanConfig{AntiSpamLimit: 25, TTLStage4Hours: 48, TTLStage6Days: 5},
		testLogger())

	conn, _, err := r.dial(r.token(t, time.Hour, auth.RoleManager))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	rd := readLoop(conn)
	warmup(t, pub, rd)

	// Оплата двигает лида 2 → 3 (§3.1: payment разрешён только в 3 и 4).
	start := time.Now()
	if _, err := machine.Transition(context.Background(), 42, 3,
		kanban.ActorPayment, "ws acceptance IQ-10"); err != nil {
		t.Fatalf("transition: %v", err)
	}
	for {
		msg := rd.next(t, time.Second)
		var got events.Event
		if err := json.Unmarshal(msg, &got); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, msg)
		}
		if got.LeadID != 42 {
			continue // хвост прогрева
		}
		elapsed := time.Since(start)
		t.Logf("transition → клиент: %v (IQ-10: < 500 мс)", elapsed)
		if elapsed >= 500*time.Millisecond {
			t.Fatalf("доставка заняла %v (IQ-10: < 500 мс)", elapsed)
		}
		if got.Type != events.TypeStageChange || got.StageID != 3 ||
			got.OldStageID == nil || *got.OldStageID != 2 ||
			got.Actor != string(kanban.ActorPayment) || got.TS.IsZero() {
			t.Fatalf("событие исказилось при доставке: %+v", got)
		}
		return
	}
}

// --- критерий 2 (AQ²-5): reconnect после expiry → catch-up ---

// Полный клиентский протокол §10.3: живое событие → close 4001 по expiry →
// (события в окно реконнекта потеряны pub/sub'ом) → новый токен →
// GET /api/leads?updated_since={last_event_ts} находит пропущенное →
// новый WS живёт дальше.
func TestReconnect_ExpiredToken_CatchUp(t *testing.T) {
	addr := testRedisAddr(t)
	r := newRig(t, DefaultTimings)
	_, pub := runHub(t, r, addr)

	t0 := time.Now().UTC().Add(-time.Hour)
	r.leads.leads[5] = &models.Lead{
		ID: 5, TelegramUserID: 500, StageID: 2,
		LastActivityAt: t0, CreatedAt: t0,
	}

	// Соединение с коротким токеном: 2с хватает на warmup + живое событие.
	conn, _, err := r.dial(r.token(t, 2*time.Second, auth.RoleManager))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	rd := readLoop(conn)
	warmup(t, pub, rd)

	// Живое событие: клиент запоминает last_event_ts (§10.3 шаг 1).
	if err := pub.Publish(context.Background(), events.Event{
		Type: events.TypeStageChange, LeadID: 5, StageID: 2, Actor: "system",
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	var lastEventTS time.Time
	for lastEventTS.IsZero() {
		var got events.Event
		if err := json.Unmarshal(rd.next(t, time.Second), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.LeadID == 5 {
			lastEventTS = got.TS
		}
	}

	// Expiry: сервер сам закрывает соединение кодом 4001 (§5.3).
	deadline := time.Now().Add(5 * time.Second)
	var closeErr error
waitClose:
	for {
		select {
		case <-rd.msgs: // поздний прогрев — пропускаем
		case closeErr = <-rd.err:
			break waitClose
		case <-time.After(time.Until(deadline)):
			t.Fatal("сервер не закрыл соединение по истечении токена")
		}
	}
	var ce *websocket.CloseError
	if !errors.As(closeErr, &ce) || ce.Code != CloseTokenExpired {
		t.Fatalf("ожидали close %d по expiry, получили: %v", CloseTokenExpired, closeErr)
	}

	// Окно реконнекта: лид обновился, событие ушло в pub/sub без
	// подписчика-клиента — потеряно навсегда (fire-and-forget).
	offlineAt := time.Now().UTC()
	r.leads.touch(5, offlineAt, 3)
	if err := pub.Publish(context.Background(), events.Event{
		Type: events.TypeStageChange, LeadID: 5, StageID: 3, Actor: "payment",
	}); err != nil {
		t.Fatalf("publish offline: %v", err)
	}

	// §10.3 шаги 2–3: новый токен (refresh-флоу закрыт тестами M7) и
	// catch-up. Строгий After: берём ts ЖИВОГО события, оно раньше offlineAt.
	fresh := r.token(t, time.Hour, auth.RoleManager)
	req, _ := http.NewRequest(http.MethodGet,
		r.srv.URL+"/api/leads?updated_since="+lastEventTS.Format(time.RFC3339Nano), nil)
	req.Header.Set("Authorization", "Bearer "+fresh)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("catch-up запрос: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("catch-up: %d %s", resp.StatusCode, body)
	}
	var page struct {
		Leads []struct {
			ID      int64 `json:"id"`
			StageID int16 `json:"stage_id"`
		} `json:"leads"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("catch-up decode: %v", err)
	}
	if len(page.Leads) != 1 || page.Leads[0].ID != 5 || page.Leads[0].StageID != 3 {
		t.Fatalf("catch-up не догнал пропущенный переход: %+v", page.Leads)
	}

	// §10.3 шаг 4: новый WS, live-события снова идут.
	conn2, _, err := r.dial(fresh)
	if err != nil {
		t.Fatalf("reconnect dial: %v", err)
	}
	defer conn2.Close()
	rd2 := readLoop(conn2)
	warmup(t, pub, rd2)
}

// Заглушки M11 (recovery-cron pending_task работает с боевым leadRepo,
// в этих тестах не участвует).
func (f *wsLeads) ListPendingTask(context.Context, int) ([]models.Lead, error) {
	panic("pending_task здесь не используется (M11)")
}

func (f *wsLeads) ClearPendingTask(context.Context, int64, int) (bool, error) {
	panic("pending_task здесь не используется (M11)")
}
