# M12 — Чат менеджера и счета из CRM

**Ветка:** `feat/m12-manager-chat`
**Зависимости:** M3 (Sender/processor), M6 (payment.Client), M8 (REST API + auth), M9 (события WS), M10 (React-доска)
**Разделы SRS:** нет — продуктовое расширение владельца (2026-07-07, после боевого запуска).
Контракты фиксируются ЭТИМ файлом и кодом; SRS v2.3 не трогаем.

## Цель
Менеджер работает с клиентом, не выходя из CRM: в карточке лида видна вся
переписка (клиент ↔ Эмма ↔ менеджер) в реальном времени, менеджер пишет
клиенту через бота и выставляет счёт CryptoBot кнопкой — ссылка уходит
клиенту в Telegram автоматически (сейчас — только терминальный
`scripts/new_invoice.sh`, ссылку пересылают руками).

## Контекст (что уже есть — переиспользовать, не дублировать)
- `MessageRepo.CreateOutbound` — «вставляет ответ бота/менеджера», счётчики
  лида не трогает (CLAUDE.md §4.3 — только inbound). `ListByLead` — история.
- `worker.Sender` (`Send(chatID int64, text string) error`) — отправка в
  Telegram; реализация на `bot.Send` уже живёт в проде (M3).
- `payment.Client.CreateInvoice(leadID, params)` — инвойс с payload=lead_id;
  оплата двигает карточку сама (боевой контур M6 проверен 2026-07-07).
- `events.Event` → Redis `crm:events` → Hub → WS: Hub ретранслирует всё,
  что попадает в канал; фронт хранит `last_event_ts` для catch-up (§10.3).
- API-паттерны M8: auth.Middleware + RequireRole(manager, admin), ошибки
  `{"error","code"}`, rate limit до auth, contract-тест с фейками.

## Задачи
1. **Миграция 0012**: `messages.author VARCHAR(32) NULL`
   (`'bot'` | `'manager:<id>'`; NULL в старых строках читать как bot).
   Нужна UI («кто написал») и прозрачности LGPD-экспорта. Schema-lint.
   Модель `models.Message` — поле Author. LGPD: erasure уже обнуляет
   `messages.content` для всех строк — author лида не идентифицирует,
   не трогать; export отдаёт author как есть.
2. **`GET /api/leads/:id/messages`** — история переписки: от старых к новым
   (порядок контекста Claude), пагинация `limit`/`before_id` (прокрутка
   вверх), roles manager|admin. 404 — нет лида (стёртый — 404, как M8).
3. **`POST /api/leads/:id/messages`** `{"text": "..."}` — написать клиенту:
   - `Sender.Send` → при успехе `CreateOutbound` (author=`manager:<sub>`
     из JWT claims) → событие WS. Порядок именно такой: Telegram отказал
     (клиент заблокировал бота и т.п.) → **502 `ERR_TELEGRAM_SEND`, в БД
     ничего не пишем** — в истории не должно быть недоставленных реплик.
   - 400 — пустой/пробельный текст; лимит длины 4096 (лимит Telegram).
4. **`POST /api/leads/:id/invoice`** `{"amount": "2000", "asset": "USDT"}`:
   - `payment.Client.CreateInvoice` (expires 24h, description по услуге —
     опциональное поле `description`);
   - бот шлёт клиенту сообщение со ссылкой (шаблон: короткий текст от Эммы
     + `BotInvoiceURL`), запись outbound (author=`manager:<sub>`), WS-событие;
   - ответ менеджеру: `{invoice_id, url}` — показать в UI;
   - если Telegram-отправка упала, а инвойс уже создан — 502 с `url` в теле
     ответа: счёт живой, менеджер перешлёт ссылку сам (в БД сообщение
     не пишем). amount — строка → decimal (как M6), 400 при мусоре.
5. **Событие WS `message`** в `events.Event`: `lead_id`, `direction`,
   `author`, `content`, `ts`. Публиковать из ТРЁХ точек записи messages:
   ingestion inbound (M2), ответ Эммы/nonTextReply (M3), ручка M12.
   Канал внутренний (Redis за паролем, WS за JWT) — полный текст допустим.
   Событие НЕ трогает stage — `StageID` заполнять текущей стадией лида
   (фронт использует для консистентности карточки).
6. **Фронт (web/)** — чат-панель в карточке лида (M10-архитектура:
   логика в `web/src/lib`, DnD/стор не ломать):
   - история по GET при открытии карточки + догрузка вверх (`before_id`);
   - живые обновления по WS `message`; при reconnect/переходе в polling —
     перезапрос истории (событие могло потеряться, catch-up §10.3);
   - пузыри: клиент / Эмма (author=bot|NULL) / менеджер — визуально различимы;
   - поле ввода + отправка (disabled на время запроса, ошибка — тостом);
   - кнопка «Выставить счёт»: сумма + валюта (USDT|USDC) + описание,
     подтверждение, после успеха ссылка видна в чате и копируется.
7. **Контекст Эммы**: сообщения менеджера уже попадут в историю Claude через
   `ListByLead` (роль assistant, как ответы Эммы). Решение M12: этого
   достаточно — Эмма «считает слова менеджера своими» и не противоречит им;
   в prompt.go ничего не менять, зафиксировать contract-тестом сборки
   промпта (outbound менеджера присутствует в messages-блоке).

## Вне скоупа (не делать в M12)
- Пауза/отключение автоответов Эммы при ручной переписке («перехват
  диалога») — отдельное продуктовое решение, вернуться после обкатки чата.
- Вложения/фото от менеджера — только текст.
- Кнопка счёта у Эммы (бот сам предлагает оплату) — после обкатки.

## Контракт наружу
- WS-событие `message` — единственный новый тип; клиенты M9/M10 без чата
  обязаны молча игнорировать незнакомые типы (проверить, что игнорируют).
- `POST /api/leads/:id/invoice` — переиспользует M6-контур целиком;
  `scripts/new_invoice.sh` остаётся как запасной терминальный путь.

## Критерии приёмки
- [x] Сообщение из карточки доходит лиду в Telegram; строка в `messages`
      (outbound, author=`manager:<id>`); чат в другой открытой вкладке
      обновляется live без перезагрузки (метод: e2e по образцу m10_e2e).
- [x] Отказ Telegram → 502 `ERR_TELEGRAM_SEND`, в `messages` записи нет
      (contract-тест с фейковым Sender).
- [x] Счёт из карточки: клиент получает ссылку в чат, менеджер видит URL
      (живой смоук testnet в e2e, E2E_INVOICE=1); оплата двигает карточку —
      контур M6 переиспользован целиком (payload=lead_id через
      payment.Client), вебхук→стадия закрыт contract-тестами M6.
- [x] `GET .../messages` — порядок от старых к новым, `before_id` листает
      назад, стёртый лид → 404 (contract- и repo-интеграционный тесты).
- [x] Эмма отвечает с учётом реплики менеджера (contract-тест prompt-сборки
      TestPromptBuild_ManagerOutboundInMessages).
- [x] Inbound-сообщения клиента тоже приходят WS-событием `message`
      (чат живой в обе стороны; contract-тест вебхука + живой e2e).
- [x] `go build ./...`, `go vet ./...`, schema-lint, `npm test` зелёные.

## Как прогнать проверки (полный чек-лист команд)

Окружение (Intel Mac, go/node установлены в ~/sdk — см. память проекта):

```bash
export PATH=$HOME/sdk/go1.22.12/bin:$HOME/sdk/node-v20.18.1-darwin-x64/bin:$PATH
export GOTOOLCHAIN=go1.22.12   # без него go.mod уезжает с go 1.22
cd /Users/amigo/aicrm/interfin-ai-crm
```

1. Статика и сборка:

```bash
go build ./... && go vet ./...
gofmt -l cmd internal            # пусто = ок (урок M6)
```

2. Миграция 0012 и schema-lint (dev-БД в docker compose):

```bash
docker compose up -d postgres redis
export POSTGRES_DSN='postgres://postgres:postgres@localhost:5432/interfin?sslmode=disable'
go run ./cmd/migrate up          # откат: down 1
go run ./cmd/schema-lint
```

3. Юнит- и contract-тесты (без внешних зависимостей — фейки):

```bash
go test ./internal/handlers ./internal/worker ./internal/events ./internal/payment
```

4. Интеграционные (Postgres+Redis). Грабли, все ловлены раньше:
   БД для тестов — **interfin_test** (тесты TRUNCATE-ят таблицы, живой
   dev-стенд не задевать); контейнер `app` ОСТАНОВИТЬ (крадёт задачи из
   очереди default); пакеты делят Redis/Postgres → `-p 1`; asynq
   unique-замки не снимаются DeleteTask — при странных фейлах очистить
   `redis-cli keys 'asynq:{default}:unique:*' | xargs redis-cli del`.

```bash
docker compose stop app
POSTGRES_TEST_DSN='postgres://postgres:postgres@localhost:5432/interfin_test?sslmode=disable' \
REDIS_TEST_ADDR=localhost:6379 \
  go test -p 1 ./internal/...
```

5. Фронт:

```bash
cd web && npm test && cd ..
```

6. E2E чата (по образцу M10: сервер поднят локально, менеджеры —
   сетап-скриптом; сценарий M12 — web/e2e/chat.e2e.test.jsx, своего лида
   он создаёт сам живым вебхуком и стирает лидов прошлых прогонов).
   Боевой Telegram не доставит фейковому лиду (chat not found), поэтому
   сервер на e2e смотрит в СТАБ Bot API (scripts/tg_stub; конфиг
   telegram.api_url, в prod/dev пуст = боевой API):

```bash
docker compose stop app          # :8080 нужен локальному серверу
go run ./scripts/tg_stub &       # фейковый Telegram Bot API на :8091
set -a; source .env; set +a
HTTP_PORT=8080 TELEGRAM_API_URL=http://localhost:8091 \
TELEGRAM_WEBHOOK_URL=http://localhost:8080/webhook/telegram \
  go run ./cmd/server &
bash scripts/m10_e2e_setup.sh    # e2e-учётки менеджеров (M12 переиспользует)
cd web && E2E_BASE=http://localhost:8080 \
  E2E_TG_STUB=http://localhost:8091 \
  E2E_TG_SECRET=$TELEGRAM_WEBHOOK_SECRET \
  E2E_INVOICE=1 npm run test:e2e && cd ..   # E2E_INVOICE=1 — живой смоук
                                            # счёта, нужен testnet-токен в .env
```

   Грабли e2e (ловлены в M12): обрыв WS имитировать `ws.terminate()`, не
   `close()` (вежливый handshake доставляет события — тест ничего не
   проверит); пузырь искать `within(chat-list)` — React зеркалит value
   textarea в textContent, глобальный findByText находит сам textarea;
   /api под rate limit 100/мин с IP — не частить поллингом и не гонять
   прогоны подряд.

7. Живой смоук счёта — ТОЛЬКО testnet (`CRYPTOBOT_USE_TESTNET=true`,
   токен в .env; тестовые TON — кран @testgiver_ton_bot):
   выставить счёт через новую ручку (curl с JWT от /auth/login) и оплатить
   в @CryptoTestnetBot; карточка должна переехать, ссылка — прийти в чат.

8. Финал — Definition of Done из CLAUDE.md §6 + критерии приёмки выше;
   эпик закрывается одним squash-коммитом в `feat/m12-manager-chat`.
