# M2 — Telegram Ingestion

**Ветка:** `feat/m2-ingestion`
**Зависимости:** M0, M1
**Разделы SRS:** §6.1 (Webhook Flow), §5.4 (Telegram Webhook Security)

## Цель
Приём входящих сообщений Telegram, сохранение inbound, постановка задачи в Asynq.
БЕЗ AI — просто «пришло → сохранили → поставили в очередь». Ответа пока нет.

## Задачи
1. Настроить telebot.v3 в **webhook-режиме** (§6.1):
   ```
   wh := &telebot.Webhook{ Listen: "",
       Endpoint: &telebot.WebhookEndpoint{ PublicURL: cfg.Telegram.WebhookURL } }
   bot, _ := telebot.NewBot(telebot.Settings{ Poller: wh })
   ```
2. Зарегистрировать через `gin.WrapH(wh)` на `POST /webhook/telegram`.
   Polling ЗАПРЕЩЁН.
3. Middleware безопасности (§5.4): проверка заголовка
   `X-Telegram-Bot-Api-Secret-Token`; при невалидном → 403.
4. Middleware `SaveAndReturn200`:
   - найти/создать `lead` по `telegram_user_id`;
   - сохранить сообщение (`direction='inbound'`), инкремент `message_count`,
     обновить `last_activity_at`;
   - вернуть **HTTP 200 немедленно**;
   - enqueue Asynq-задачу `process:inbound` с `TaskID(hash)` + `Unique` (§6.3).
5. Настроить Asynq client (пока без обработчика — воркер в M3).
6. Регистрация webhook в Telegram при старте (setWebhook).
7. Для локальной разработки: инструкция по ngrok (§13.1) в README.

## Контракт наружу
- Гарантия: каждое входящее сообщение сохранено в `messages` до ответа 200.
- Задача `process:inbound{lead_id, msg_id}` в очереди Asynq — вход для M3.
- Дедуп-ключ `sha256(lead_id:msg_id)`.

## Критерии приёмки
- [ ] Webhook возвращает 200 за < 300 мс независимо от последующей обработки (IQ-1).
- [ ] Запрос без валидного Secret-Token → 403 (§5.4).
- [ ] `message_count` растёт только на inbound (IQ-2).
- [ ] Дубль одного апдейта → одна задача в очереди (AQ²-6, проверяется с M3).
- [ ] Polling нигде не используется.
