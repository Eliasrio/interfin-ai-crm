# Фаза 0 — получение боевых ключей (пошагово, со ссылками)

Эти шаги выполняет владелец продукта ДО провижининга сервера
([deploy_steps.md](deploy_steps.md)). Итог фазы — 6 позиций в менеджере
паролей команды. Время: ~1 час (не считая ожидания домена/карты).

---

## 1. Боевой Telegram-бот

Дев-бот остаётся для разработки/стейджинга; в прод идёт ОТДЕЛЬНЫЙ бот —
иначе каждый перезапуск dev с ngrok перехватывал бы боевой вебхук.

1. Открыть [@BotFather](https://t.me/BotFather) → `/newbot`.
2. Имя — то, что увидят лиды (например, «INTERFIN Consultoria»); username —
   уникальный, оканчивается на `bot` (например, `interfin_crm_bot`).
3. BotFather пришлёт **токен** вида `1234567890:AA...` → в менеджер паролей
   как `TELEGRAM_BOT_TOKEN`.
4. Косметика (по желанию): `/setuserpic`, `/setdescription` — это видит лид
   при первом открытии чата.
5. **Секрет вебхука** сгенерировать самостоятельно: `openssl rand -hex 32` →
   `TELEGRAM_WEBHOOK_SECRET`. (Регистрирует вебхук приложение само при
   старте — руками ничего вызывать не нужно.)

Справка по ботам: https://core.telegram.org/bots/features

## 2. Chat ID чата менеджеров

Сюда бот шлёт алерты: dead letter (§6.3), эскалации anti-spam (§3.5) и
уведомления Alertmanager (M11).

1. Создать группу в Telegram («INTERFIN CRM Алерты»), добавить менеджеров.
2. Добавить в группу БОЕВОГО бота из шага 1 (права: только отправка сообщений).
3. Узнать id группы — любой способ:
   - добавить [@getidsbot](https://t.me/getidsbot) в группу — он напишет id
     и его можно сразу удалить; либо
   - написать что-нибудь в группу и открыть в браузере
     `https://api.telegram.org/bot<токен>/getUpdates` — в ответе
     `"chat":{"id":-100...}`.
4. Записать как `TELEGRAM_MANAGER_CHAT_ID`. У супергрупп id отрицательный
   (`-100...`) — знак минус обязателен.

## 3. Ключ Anthropic (Claude)

Модель проекта — `claude-sonnet-5` (актуальная, контекст 1M; до 2026-08-31
действует вводная цена $2/$10 за MTok, далее $3/$15).

1. Консоль: https://console.anthropic.com/ (аккаунт организации, не личный).
2. Биллинг: https://console.anthropic.com/settings/billing — привязать карту
   / купить кредиты. Без биллинга ключ отдаёт 400.
3. Создать ключ: https://console.anthropic.com/settings/keys →
   **Create Key**, имя `interfin-crm-prod` → `ANTHROPIC_API_KEY`
   (`sk-ant-...`, показывается один раз).
4. Лимиты: https://console.anthropic.com/settings/limits — убедиться, что
   tier даёт запас по RPM/TPM (нагрузка CRM: 1 вызов Claude на входящее
   сообщение лида, ответ ≤1000 токенов, запрос ≤9000).
5. Цены/модели для сверки: https://platform.claude.com/docs/en/about-claude/models/overview

Дев-ключ в прод не переносить — отдельный ключ проще ротировать и считать.

## 4. Ключ Voyage AI (эмбеддинги RAG)

Модель — `voyage-3` (§7.1, AQ²-2: НЕ OpenAI).

1. Дашборд: https://dashboard.voyageai.com/ → API Keys → создать ключ →
   `VOYAGE_API_KEY`.
2. **Обязательно привязать карту** (Billing в дашборде): free tier —
   3 запроса/мин и 10K токенов/мин (проверено в M4 — smoke-тесты упирались
   в 429). Прод-лимиты включаются после привязки карты.
   Документация по лимитам: https://docs.voyageai.com/docs/rate-limits
3. Помнить: индексация базы знаний (`index-kb`) и калибровка порога
   (`rag_calibrate`) тоже ходят этим ключом.

## 5. CryptoBot mainnet (крипто-платежи)

Testnet-приложение из разработки остаётся (переменная обязательна),
но платежи в проде идут через **mainnet**.

1. Открыть [@CryptoBot](https://t.me/CryptoBot) (НЕ @CryptoTestnetBot) →
   `Crypto Pay` → `Create App`.
2. Получить **API Token** → `CRYPTOBOT_MAINNET_TOKEN`.
3. В настройках приложения (`My Apps` → приложение → `Webhooks`) указать
   endpoint: `https://crm.<домен>/webhook/payment` — именно этот путь
   слушает приложение (M6). Сделать это можно после фазы 7 деплоя,
   когда домен уже отвечает.
4. Документация Crypto Pay API: https://help.crypt.bot/crypto-pay-api
5. Учесть продуктовый факт из M6: комиссия CryptoBot 3% — tolerance
   недоплаты в конфиге уже поднят до 3% (`underpaid_tolerance_pct`), при
   изменении комиссии шлюза его надо пересматривать.

## 6. Домен

1. Купить домен у любого регистратора (для Бразилии/LGPD-контекста уместен
   и `.com.br` — https://registro.br/, и обычный `.com` — Namecheap/
   Cloudflare Registrar).
2. Завести A-запись `crm.<домен>` → Elastic IP сервера (появится в фазе 1
   деплоя). TTL 300.
3. Если DNS у Cloudflare — режим **DNS only** (серая тучка), НЕ Proxied:
   за прокси Cloudflare сломается mTLS-защита `/metrics`, а certbot
   standalone не пройдёт ACME-челлендж на 80-м порту.

---

## Чек-лист готовности фазы 0

В менеджере паролей команды лежат:

- [ ] `TELEGRAM_BOT_TOKEN` (боевой бот) + `TELEGRAM_WEBHOOK_SECRET` (сгенерирован)
- [ ] `TELEGRAM_MANAGER_CHAT_ID` (отрицательный id группы с ботом внутри)
- [ ] `ANTHROPIC_API_KEY` (prod, биллинг активен)
- [ ] `VOYAGE_API_KEY` (карта привязана — не free tier)
- [ ] `CRYPTOBOT_MAINNET_TOKEN` (+ дев-`CRYPTOBOT_TESTNET_TOKEN` под рукой)
- [ ] Домен куплен, доступ к DNS-панели есть

Дальше — [deploy_steps.md](deploy_steps.md), фаза 1.
