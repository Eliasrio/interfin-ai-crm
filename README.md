# Interfin AI-CRM — Dev Kit для Claude Code

Пакет для поэтапной разработки Interfin AI-CRM силами Claude Code под
руководством нетехнического менеджера. Основан на `SRS_v2.3.docx`.

## Как пользоваться

1. Положи этот каталог в корень репозитория (рядом с кодом).
2. `CLAUDE.md` читается Claude Code автоматически — держи его на месте.
3. Разработку веди **по одному эпику за сессию**, в порядке M0 → M11.
4. В начале сессии скажи Claude Code, например:
   > «Работаем над эпиком M2. Открой `tasks/M2_telegram_ingestion.md` и
   > `CLAUDE.md`, реализуй по задачам, следуй жёстким правилам, закрой
   > критерии приёмки.»
5. Не грузи всё ТЗ разом — каждый task-файл указывает, какие разделы SRS нужны.

## Эпики

| Эпик | Файл | Зависит от | Что делает |
|---|---|---|---|
| M0 | `M0_skeleton.md` | — | Скелет: config, docker, миграции, health |
| M1 | `M1_data_layer.md` | M0 | БД: 5 таблиц, GORM, pgx, schema-lint |
| M2 | `M2_telegram_ingestion.md` | M0,M1 | Приём Telegram → сохранение → очередь |
| M3 | `M3_worker_claude.md` | M0,M1,M2 | Воркер + Claude, диалог, токен-бюджет |
| M4 | `M4_rag.md` | M1,M3 | RAG (Voyage + pgvector), summary |
| M5 | `M5_kanban_state_machine.md` | M1,M3 | 8 стадий, anti-spam+TTL, переходы |
| M6 | `M6_payment.md` | M1,M5 | USDT webhook, tolerance, авто-стадии |
| M7 | `M7_auth.md` | M0,M1 | JWT RS256, refresh, роли |
| M8 | `M8_rest_api.md` | M1,M5,M7 | REST API, catch-up, LGPD |
| M9 | `M9_websocket.md` | M5,M7,M8 | WebSocket, catch-up, heartbeat |
| M10 | `M10_react_kanban.md` | M8,M9 | React-доска Kanban |
| M11 | `M11_ha_monitoring.md` | всё | Sentinel, WAL, Prometheus, deploy |

## Порядок и параллелизм

```
M0 ─▶ M1 ─┬─▶ M2 ─▶ M3 ─▶ M4
          │              │
          │              ▼
          │        M5 ─▶ M6
          │
          └─▶ M7 ─▶ M8 ─▶ M9 ─▶ M10
                                 │
              M11 ◀──────────────┘
```

- **M0, M1 — строго первыми.**
- **M7** можно вести параллельно ветке M2→M3→M4.
- **M10** — только после M8 и M9.
- **M11** — последним.

## Definition of Done (общий для всех)

- [ ] Все задачи эпика реализованы.
- [ ] Критерии приёмки эпика проходят.
- [ ] Жёсткие правила из `CLAUDE.md` §4 не нарушены.
- [ ] `go build ./...` и `go vet ./...` чисто.
- [ ] Изменения в ветке эпика закоммичены.

## Слой данных (M1)

```bash
docker compose up -d postgres
go run ./cmd/migrate up          # накатить схему (down 1 / down all — откат)
go run ./cmd/schema-lint         # модели ↔ миграции (AQ²-1), гоняется в CI
POSTGRES_TEST_DSN=postgres://postgres:postgres@localhost:5432/interfin?sslmode=disable \
  go test ./internal/repo        # интеграционные тесты (без DSN — skip)
```

Матрица «поле ↔ таблица ↔ где используется» — `docs/field_schema_matrix.md`.

## Telegram webhook локально (M2, ngrok — SRS §13.1)

Telegram доставляет апдейты только на публичный HTTPS-URL, поэтому для
локальной разработки нужен туннель (в dev-матрице окружений это ngrok):

```bash
# 1. Зависимости и схема
docker compose up -d postgres redis
export POSTGRES_DSN='postgres://postgres:postgres@localhost:5432/interfin?sslmode=disable'
go run ./cmd/migrate up

# 2. Туннель на порт приложения
ngrok http 8080
# скопируй выданный https-адрес, например https://a1b2c3.ngrok-free.app

# 3. Окружение (или заполни .env по .env.example)
export REDIS_ADDR=localhost:6379
export TELEGRAM_BOT_TOKEN='<токен от @BotFather>'
export TELEGRAM_WEBHOOK_SECRET="$(openssl rand -hex 32)"
export TELEGRAM_WEBHOOK_URL='https://a1b2c3.ngrok-free.app/webhook/telegram'

# 4. Старт: сервер сам зарегистрирует webhook (setWebhook) при запуске
go run ./cmd/server
```

Проверка: напиши боту в Telegram — в логе появится `webhook: создан новый
лид`, сообщение окажется в таблице `messages` (`direction='inbound'`),
а задача `process:inbound` — в очереди Asynq (обрабатывать её начнёт
воркер M3). Ответа от бота на этом этапе НЕТ — это по плану.

Важно:
- URL в `TELEGRAM_WEBHOOK_URL` — полный, вплоть до `/webhook/telegram`.
- Каждый перезапуск ngrok меняет адрес — обнови переменную и перезапусти
  сервер (он перерегистрирует webhook).
- Запросы без валидного `X-Telegram-Bot-Api-Secret-Token` получают 403 —
  секрет обязателен, «открытого» webhook в проекте нет (§5.4).
- Polling запрещён (CLAUDE.md §4.10): бот работает только через webhook,
  проверить текущую регистрацию можно методом Bot API `getWebhookInfo`.

Интеграционный тест дедупликации очереди (AQ²-6) гоняется при заданном
`REDIS_TEST_ADDR` (без него — skip):

```bash
REDIS_TEST_ADDR=localhost:6379 go test ./internal/queue
```

## Платёжный вебхук CryptoBot локально (M6, §3.3/§5.5)

Шлюз — Crypto Pay API от @CryptoBot. Сеть выбирает `CRYPTOBOT_USE_TESTNET`:
`true` — testnet (@CryptoTestnetBot, разработка), `false` — mainnet (боевой).
Токен и base URL API переключаются вместе (`internal/payment`).

Реальный поток: инвойс создаётся через `payment.Client.CreateInvoice`
(в `payload` кладётся `lead_id` — по нему вебхук связывает оплату с лидом),
лид оплачивает его в Crypto Bot, шлюз шлёт `invoice_paid` на
`POST /webhook/payment`. Дальше автоматика §3.3: `net_received ≥ 98%` →
Stage 3, недоплата → `manual_resolution` + Stage 4 (TTL 48ч).

Эмуляция вебхука без шлюза (подпись — HMAC-SHA256 тела ключом
SHA256(активный токен), заголовок `Crypto-Pay-Api-Signature`):

```bash
BODY='{"update_id":1,"update_type":"invoice_paid","request_date":"'$(date -u +%Y-%m-%dT%H:%M:%SZ)'","payload":{"invoice_id":1,"status":"paid","asset":"USDT","amount":"100","fee_asset":"USDT","fee_amount":1,"payload":"<LEAD_ID>"}}'
SIG=$(python3 -c "import hashlib,hmac,os,sys; token=os.environ['CRYPTOBOT_TESTNET_TOKEN'].encode(); print(hmac.new(hashlib.sha256(token).digest(), sys.argv[1].encode(), hashlib.sha256).hexdigest())" "$BODY")
curl -s -X POST localhost:8080/webhook/payment -H "Crypto-Pay-Api-Signature: $SIG" -d "$BODY"
```

Повтор того же `update_id` в течение 10 минут → 403 (replay-защита §5.5);
`request_date` старше ±5 минут → 403.

## Аутентификация (M7, §5.1–5.3)

JWT RS256 (access, TTL 15 мин) + opaque refresh-токен (UUID, TTL 7 дней,
HttpOnly cookie, ротация на каждый refresh). Ключи — ТОЛЬКО файлами
(Docker secrets, CLAUDE.md §4.9), пароли — только bcrypt-хешами.

```bash
# 1. Ключи RS256 (однократно; кладутся в ./secrets/, они в .gitignore).
#    docker compose монтирует их как secrets, локальному запуску нужны
#    JWT_PRIVATE_KEY_PATH/JWT_PUBLIC_KEY_PATH в .env (см. .env.example).
./scripts/gen_jwt_keys.sh

# 2. Первая учётка (пароль спросит со stdin, в БД — только bcrypt-хеш)
POSTGRES_DSN=postgres://postgres:postgres@localhost:5432/interfin?sslmode=disable \
  go run ./cmd/create-manager -email admin@interfin.com -name "Admin" -role admin

# 3. Логин: access-токен в теле, refresh — в HttpOnly cookie
curl -s -c /tmp/jar -X POST localhost:8080/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@interfin.com","password":"<пароль>"}'
# → {"access_token":"eyJ...","expires_in":900}

# 4. Продление — ТОЛЬКО по refresh-cookie (работает при истёкшем access);
#    старый refresh при этом гасится (ротация), cookie обновляется.
curl -s -b /tmp/jar -c /tmp/jar -X POST localhost:8080/auth/refresh
```

Контракт наружу: `auth.Middleware(verifier)` + `auth.RequireRole(...)`
оборачивают REST-роуты (M8); `verifier.VerifyWSProtocol` валидирует JWT из
`Sec-WebSocket-Protocol: Bearer.<token>` при upgrade (M9, просрочка →
close 4001). Просроченный access → 401 `ERR_TOKEN_EXPIRED`, чужая роль →
403 `ERR_FORBIDDEN` (AQ²-2).

## REST API менеджера (M8, §4)

Все `/api/*` — за цепочкой: rate limit 100 req/min per IP (§4.2, 429
`ERR_RATE_LIMITED`) → Bearer JWT (M7) → роли manager/admin (§5.2). Ошибки —
`{"error","code"}` + статус (§4.2). Секрет `LGPD_SALT` обязателен (соль
хеша `telegram_user_id` при erasure §9.3; `openssl rand -hex 32`, в prod
не менять после первых erasure).

```bash
TOKEN=...  # access_token из /auth/login

# Список: пагинация + catch-up для M9 (§10.3, строгое «позже», по индексу)
curl -s 'localhost:8080/api/leads?limit=50&offset=0' -H "Authorization: Bearer $TOKEN"
curl -s 'localhost:8080/api/leads?updated_since=2026-07-06T12:00:00Z' -H "Authorization: Bearer $TOKEN"

# Карточка (лид + диалог + платежи) и текущая стадия (polling fallback §10.1)
curl -s localhost:8080/api/leads/1 -H "Authorization: Bearer $TOKEN"
curl -s localhost:8080/api/leads/1/stage -H "Authorization: Bearer $TOKEN"

# Ручная смена стадии — через state machine M5 (ActorManager):
# запрещённый переход → 400 ERR_INVALID_TRANSITION, гонка → 409 ERR_STAGE_CONFLICT
curl -s -X PATCH localhost:8080/api/leads/1/stage \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"stage_id":5}'

# LGPD (§9): erasure — PII=NULL, messages='[DELETED]', telegram_user_id → хеш
# (НЕ NULL), payment_events НЕ трогаются (фиск. retention 5 лет); export — JSON
# messages + payment_events + lgpd_audit. Оба пишут след в lgpd_audit.
curl -s -X DELETE localhost:8080/api/lgpd/leads/1/erase -H "Authorization: Bearer $TOKEN"
curl -s localhost:8080/api/lgpd/leads/1/export -H "Authorization: Bearer $TOKEN"
```

Retention-cron §9.1: `lgpd:retention` (asynq scheduler, 04:00 UTC ежедневно)
физически удаляет лидов с `deleted_at` старше 90 дней; платежи выживают
с `lead_id=NULL` (FK `ON DELETE SET NULL`, миграция 0010).

## React-доска Kanban (M10, §10)

Фронт живёт в `web/` (React 18 + Vite, зависимости пришпилены точно).
Node 20 ставится как Go — вручную в `~/sdk`
(`export PATH=$HOME/sdk/node-v20.18.1-darwin-x64/bin:$PATH`).

```bash
cd web && npm install

# Dev: Vite на :5173, прокси /api /auth /ws на :8080 (docker compose up -d)
npm run dev

# Prod: собрать и отдать тем же Go-процессом (server.static_dir: web/dist;
# каталога нет — статика молча выключена, API не задет)
npm run build

# Юнит/компонентные тесты (DnD→PATCH, тихий refresh, WS-клиент, LGPD-гейт)
npm test

# e2e-приёмка против живого бэкенда (IQ-10 <500мс, AQ²-5 catch-up, DnD, LGPD).
# Без E2E_BASE прогон скипается (конвенция POSTGRES_TEST_DSN/REDIS_TEST_ADDR).
../scripts/m10_e2e_setup.sh   # тестовые менеджеры e2e-m10-* + лид
E2E_BASE=http://localhost:8080 npm run test:e2e
```

Как это устроено: access-токен ТОЛЬКО в памяти (refresh — HttpOnly-cookie
на /auth); WS `/ws/kanban` с `Sec-WebSocket-Protocol: Bearer.<token>`,
обрыв → §10.3 (refresh → `GET /api/leads?updated_since=last_event_ts` →
resubscribe); Redis down → Hub шлёт `polling_mode` и доска опрашивает REST
раз в 5 с (как и при полной недоступности WS). Логика без React — в
`web/src/lib/` (store/socket/api/auth), компоненты — в `web/src/components/`.

## Про критерии приёмки

Метки `IQ-N` / `AQ²-N` в критериях ссылаются на review-историю ТЗ (баги,
которые уже ловили). Прохождение этих тестов = защита от регресса.
Полная таблица критериев — в §16 SRS.
