// Package telegram — сборка telebot-бота в webhook-режиме.
//
// Жёсткое правило CLAUDE.md §4.10: в prod НИКАКОГО polling — только webhook
// через gin.WrapH. Poller здесь всегда *telebot.Webhook с пустым Listen:
// свой listener telebot не поднимает, маршрут обслуживает gin.
//
// Почему НЕ используется штатная связка bot.Start() + wh.ServeHTTP
// (проверено эмпирически на telebot v3.3.8):
//   - wh.ServeHTTP пишет апдейт в канал, консюмер которого появляется только
//     внутри bot.Start(); запрос, пришедший до этого (или после падения
//     setWebhook внутри Poll), навсегда виснет на отправке в nil-канал;
//   - bot.Stop() при Listen=="" паникует double close of stop
//     (bot.go:239 close(stop) + webhook.go:155 close(stop)).
//
// Вместо этого: RegisterWebhook() явно вызывает setWebhook при старте
// (задача M2 §6, фатально при ошибке), а Dispatcher синхронно отдаёт апдейты
// в bot.ProcessUpdate — официальную точку диспетчеризации telebot
// («A started bot calls this function automatically»).
package telegram

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	tele "gopkg.in/telebot.v3"

	"github.com/interfin/interfin-ai-crm/internal/config"
)

// NewWebhookBot создаёт бота с webhook-poller'ом по SRS §6.1.
//
// offline=true — для тестов: telebot не ходит в Telegram API (getMe).
func NewWebhookBot(cfg config.TelegramConfig, log *slog.Logger, offline bool) (*tele.Bot, error) {
	if !offline && cfg.BotToken == "" {
		return nil, errors.New("telegram: пустой bot_token (TELEGRAM_BOT_TOKEN)")
	}
	if cfg.WebhookURL == "" {
		return nil, errors.New("telegram: пустой webhook_url (TELEGRAM_WEBHOOK_URL)")
	}

	wh := &tele.Webhook{
		Listen: "", // listener не наш: маршрут отдаёт gin (SRS §6.1)
		Endpoint: &tele.WebhookEndpoint{
			PublicURL: cfg.WebhookURL,
		},
		// Telegram будет присылать X-Telegram-Bot-Api-Secret-Token;
		// его сверяет handlers.SecretToken (403 при невалидном, §5.4).
		SecretToken: cfg.WebhookSecret,
	}

	bot, err := tele.NewBot(tele.Settings{
		// URL пустой = боевой api.telegram.org (telebot-дефолт); стаб
		// scripts/tg_stub подставляется ТОЛЬКО в e2e M12 через TELEGRAM_API_URL.
		URL:     cfg.APIURL,
		Token:   cfg.BotToken,
		Poller:  wh, // webhook-режим; polling запрещён (CLAUDE.md §4.10)
		Offline: offline,
		OnError: func(err error, _ tele.Context) {
			log.Error("telebot", "error", err)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("telegram: создание бота: %w", err)
	}
	return bot, nil
}

// RegisterWebhook — регистрация webhook в Telegram при старте (задача M2 §6).
// Вызывается синхронно из main: если setWebhook не прошёл, ingestion мёртв —
// приложению нельзя молча работать дальше.
func RegisterWebhook(bot *tele.Bot) error {
	wh, ok := bot.Poller.(*tele.Webhook)
	if !ok {
		return errors.New("telegram: poller обязан быть *telebot.Webhook (CLAUDE.md §4.10)")
	}
	if err := bot.SetWebhook(wh); err != nil {
		return fmt.Errorf("telegram: setWebhook: %w", err)
	}
	return nil
}

// Dispatcher — http.Handler для gin.WrapH(...): финальное звено цепочки
// POST /webhook/telegram. Синхронно раскладывает апдейт по хендлерам бота.
// В M2 хендлеров нет (ответы лидам появятся в M3 через Asynq-воркер), поэтому
// будущие bot.Handle обязаны оставаться лёгкими: dispatch идёт в goroutine
// HTTP-запроса, уже после того как middleware зафиксировал 200.
type Dispatcher struct {
	bot *tele.Bot
}

func NewDispatcher(bot *tele.Bot) *Dispatcher {
	return &Dispatcher{bot: bot}
}

func (d *Dispatcher) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	var upd tele.Update
	if err := json.NewDecoder(r.Body).Decode(&upd); err != nil {
		// Нечитаемые тела уже отсёк SaveAndReturn200; сюда не доходят.
		return
	}
	d.bot.ProcessUpdate(upd)
}
