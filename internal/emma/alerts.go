// alerts.go — алерты панели Эммы в Telegram-группу владельца (EP-06,
// ТЗ §3 вкладка 6): Notifier со счётчиками-сериями в Redis и роуты
// GET/PATCH /api/emma/alerts + POST /api/emma/alerts/test.
//
// Redis здесь FAIL-OPEN — ПРОТИВОПОЛОЖНО PIN-контуру (store.go, §2.2):
// у PIN ошибка Redis закрывает панель (503), потому что деградация
// «пустить без проверки» опасна; у алертов деградация «Эмма работает,
// алертов нет» допустима (task §критерии) — любая ошибка Redis только
// slog.Warn, диалог и запись событий не страдают. Никаких каскадов:
// ошибка самого алертинга не порождает алертов и не роняет вызывающего.
package emma

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/settings"
)

// Sender — отправка алерта в Telegram. Узкий срез worker.Sender: алерты
// шлёт САМА Эмма (решение владельца, ТЗ §0/§10 п.4) — в проде это тот же
// TelebotSender, что отвечает лидам; контур уведомлений менеджерам M13
// (ManagerChatID) не задействован.
type Sender interface {
	Send(chatID int64, text string) error
}

// Ключи Redis — приватные для alerts.go (контракт task «наружу»).
const (
	// alertStreakPrefix — серия ошибок подряд: emma:alert:streak:<kind>.
	// Сбрасывается успешным reply (OnSuccess); страховочный TTL сутки —
	// чтобы ключ не жил вечно, если Эмму просто перестали спрашивать.
	alertStreakPrefix = "emma:alert:streak:"
	// alertCooldownPrefix — анти-шум: emma:alert:cooldown:<kind> SET NX
	// EX 900 — не чаще одного алерта одного типа в 15 минут (ТЗ §3).
	alertCooldownPrefix = "emma:alert:cooldown:"
)

const (
	// alertStreakThreshold — серий из скольких ошибок подряд достаточно
	// для алерта (llm_api и telegram_api; kb_index алертит каждый раз).
	alertStreakThreshold = 3
	// alertCooldown — окно анти-шума на тип проблемы.
	alertCooldown = 15 * time.Minute
	// alertStreakTTL — страховочный TTL счётчика серии.
	alertStreakTTL = 24 * time.Hour
)

// alertTitles — человекочитаемый «тип проблемы» в тексте алерта.
var alertTitles = map[string]string{
	models.EmmaErrLLMAPI:      "3 ошибки Claude API подряд",
	models.EmmaErrTelegramAPI: "3 ошибки Telegram API подряд",
	models.EmmaErrKBIndex:     "ошибка индексации базы знаний",
}

// Notifier — контур алертов (реализует worker.AlertSink). Все методы
// best effort by design: ошибок не возвращают, внутри только slog.
type Notifier struct {
	rdb      redis.UniversalClient
	settings Settings
	sender   Sender
	log      *slog.Logger
	now      func() time.Time // подменяется в тестах (время в тексте алерта)
}

func NewNotifier(rdb redis.UniversalClient, st Settings, sender Sender, log *slog.Logger) *Notifier {
	return &Notifier{rdb: rdb, settings: st, sender: sender, log: log, now: time.Now}
}

// OnSuccess — успешный ответ Эммы: серии llm_api/telegram_api обнуляются
// (ТЗ §3: «счётчик в Redis, сбрасывается успешным ответом»).
func (n *Notifier) OnSuccess(ctx context.Context) {
	err := n.rdb.Del(ctx,
		alertStreakPrefix+models.EmmaErrLLMAPI,
		alertStreakPrefix+models.EmmaErrTelegramAPI).Err()
	if err != nil {
		// fail-open: без Redis живём без алертов, Эмма работает.
		n.log.Warn("emma: alerts: серия не сброшена (Redis недоступен)", "error", err)
	}
}

// OnError — учёт ошибки kind. llm_api/telegram_api копят серию и алертят
// на пороге; kb_index алертит каждый раз; прочие виды (timeout,
// file_not_found — условий ТЗ на них нет) игнорируются.
func (n *Notifier) OnError(ctx context.Context, kind, detail string) {
	switch kind {
	case models.EmmaErrLLMAPI, models.EmmaErrTelegramAPI:
		if !n.bumpStreak(ctx, kind) {
			return
		}
	case models.EmmaErrKBIndex:
		// без серии — каждая ошибка индексации достойна алерта (task §5)
	default:
		return
	}
	n.alert(ctx, kind, detail)
}

// bumpStreak — INCR + ExpireNX (паттерн rate-limit M8 / PIN IncrFail);
// true — порог серии достигнут. Ошибка Redis → false (fail-open).
func (n *Notifier) bumpStreak(ctx context.Context, kind string) bool {
	key := alertStreakPrefix + kind
	count, err := n.rdb.Incr(ctx, key).Result()
	if err != nil {
		n.log.Warn("emma: alerts: серия не посчитана (Redis недоступен)",
			"kind", kind, "error", err)
		return false
	}
	if err := n.rdb.ExpireNX(ctx, key, alertStreakTTL).Err(); err != nil {
		n.log.Warn("emma: alerts: TTL серии не взведён", "kind", kind, "error", err)
	}
	return count >= alertStreakThreshold
}

// alert — анти-шум + отправка. chat_id пуст — алерты молча выключены
// (ТЗ §3: «пусто = алерты выключены»); серия при этом всё равно копится —
// включивший алерты владелец узнает о продолжающемся сбое.
func (n *Notifier) alert(ctx context.Context, kind, detail string) {
	chatID, ok := n.chatID(ctx)
	if !ok {
		return
	}
	// Анти-шум: SET NX EX 900 — второй алерт того же типа в окне гасится.
	won, err := n.rdb.SetNX(ctx, alertCooldownPrefix+kind, "1", alertCooldown).Result()
	if err != nil {
		n.log.Warn("emma: alerts: анти-шум не проверен (Redis недоступен)",
			"kind", kind, "error", err)
		return
	}
	if !won {
		return // алерт этого типа уже уходил в последние 15 минут
	}
	if err := n.sender.Send(chatID, alertText(kind, detail, n.now())); err != nil {
		// Только лог: Telegram-сбой алертинга не порождает каскадов.
		n.log.Error("emma: алерт не доставлен", "kind", kind, "chat_id", chatID, "error", err)
		return
	}
	n.log.Info("emma: алерт отправлен", "kind", kind, "chat_id", chatID)
}

// chatID — emma_panel.alert_chat_id из settings (кэш 30 с — правка из
// панели доезжает без рестарта). Пусто либо мусор — алерты выключены.
func (n *Notifier) chatID(ctx context.Context) (int64, bool) {
	raw := strings.TrimSpace(n.settings.String(ctx, settings.KeyAlertChatID))
	if raw == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		n.log.Warn("emma: alerts: alert_chat_id в settings не число", "value", raw)
		return 0, false
	}
	return id, true
}

// alertText — формат ТЗ §3: «⚠️ Эмма: <тип проблемы>\nВремя: <UTC>\n
// Последняя ошибка: <detail>».
func alertText(kind, detail string, now time.Time) string {
	title, ok := alertTitles[kind]
	if !ok {
		title = kind
	}
	return fmt.Sprintf("⚠️ Эмма: %s\nВремя: %s\nПоследняя ошибка: %s",
		title, now.UTC().Format("2006-01-02 15:04:05 UTC"), detail)
}

// --- Роуты /api/emma/alerts (настройка вкладки 6) ---

// AlertsDeps — зависимости роутов alerts.
type AlertsDeps struct {
	Settings Settings
	Sender   Sender
	Log      *slog.Logger
}

// AlertsHandler — GET/PATCH alerts, POST alerts/test.
type AlertsHandler struct {
	deps AlertsDeps
}

func NewAlerts(deps AlertsDeps) *AlertsHandler {
	return &AlertsHandler{deps: deps}
}

// Register вешает роуты на защищённую группу /api/emma (контракт EP-01).
func (h *AlertsHandler) Register(g gin.IRouter) {
	g.GET("/alerts", h.get)
	g.PATCH("/alerts", h.patch)
	g.POST("/alerts/test", h.test)
}

// alertsJSON — состояние настройки: enabled = chat_id непуст (task §5).
func (h *AlertsHandler) alertsJSON(ctx context.Context) gin.H {
	chatID := strings.TrimSpace(h.deps.Settings.String(ctx, settings.KeyAlertChatID))
	return gin.H{"chat_id": chatID, "enabled": chatID != ""}
}

// GET /api/emma/alerts.
func (h *AlertsHandler) get(c *gin.Context) {
	c.JSON(http.StatusOK, h.alertsJSON(c.Request.Context()))
}

// patchAlertsReq — chat_id строкой: int64 не влезает в JSON-число без
// потерь у JS-фронта, а пустая строка легально выключает алерты.
type patchAlertsReq struct {
	ChatID *string `json:"chat_id"`
}

// PATCH /api/emma/alerts — chat_id: int64 (группы — отрицательный),
// пусто = выключить, мусор («abc») → 400 (критерий приёмки).
func (h *AlertsHandler) patch(c *gin.Context) {
	var req patchAlertsReq
	if err := c.ShouldBindJSON(&req); err != nil || req.ChatID == nil {
		apiError(c, http.StatusBadRequest, "нужно поле chat_id (строка)", codeValidation)
		return
	}
	chatID := strings.TrimSpace(*req.ChatID)
	if chatID != "" {
		if _, err := strconv.ParseInt(chatID, 10, 64); err != nil {
			apiError(c, http.StatusBadRequest,
				"chat_id должен быть целым числом (id чата/группы) либо пустым", codeValidation)
			return
		}
	}
	ctx := c.Request.Context()
	if err := h.deps.Settings.SetString(ctx, settings.KeyAlertChatID, chatID); err != nil {
		h.deps.Log.Error("emma: alerts patch", "error", err)
		apiError(c, http.StatusInternalServerError, "внутренняя ошибка", codeInternal)
		return
	}
	h.deps.Log.Info("emma: alert_chat_id обновлён", "enabled", chatID != "")
	c.JSON(http.StatusOK, h.alertsJSON(ctx))
}

// POST /api/emma/alerts/test — пробный алерт в настроенный chat_id:
// 400 при пустом chat_id, 502 при отказе Telegram (владелец сразу видит,
// что chat_id кривой — критерий приёмки).
func (h *AlertsHandler) test(c *gin.Context) {
	ctx := c.Request.Context()
	raw := strings.TrimSpace(h.deps.Settings.String(ctx, settings.KeyAlertChatID))
	if raw == "" {
		apiError(c, http.StatusBadRequest,
			"chat_id не настроен — сначала сохраните его", codeValidation)
		return
	}
	chatID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		apiError(c, http.StatusBadRequest,
			"сохранённый chat_id повреждён — сохраните его заново", codeValidation)
		return
	}
	if err := h.deps.Sender.Send(chatID, "✅ Тестовый алерт панели Эммы"); err != nil {
		h.deps.Log.Warn("emma: тестовый алерт не доставлен", "chat_id", chatID, "error", err)
		apiError(c, http.StatusBadGateway,
			"Telegram отказал в отправке — проверьте chat_id и что Эмма есть в чате", "ERR_TELEGRAM")
		return
	}
	c.JSON(http.StatusOK, gin.H{"sent": true})
}
