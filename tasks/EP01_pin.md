# EP-01 — Панель Эммы: миграции, строковые settings, PIN, каркас API

**Ветка:** `feat/ep01-pin` (от `feat/m11-ops`)
**Зависимости:** M1 (миграции/schema-lint), M7 (auth, RequireRole), M8 (роуты
/api, конвенция ошибок), M13 (таблица settings + кэш-паттерн).
**Разделы ТЗ:** `docs/EMMA_PANEL_TZ_v2.md` §0, §2 (доступ и безопасность),
§5 (схема БД), §6 (только группа pin/* и status), §9 п.1–2.

## Цель
Фундамент панели управления Эммой: пять новых таблиц в БД, строковые ключи
в settings, полный контур PIN-защиты (6 цифр, bootstrap, блокировка,
fail-closed) и каркас `/api/emma/*` с middleware «admin + PIN-сессия»,
на который EP-02…EP-06 навешивают свои ручки.

## Контекст (что уже есть — переиспользовать, не дублировать)
- `internal/settings` (M13) — key/value с кэшем 30с и валидацией по списку
  известных ключей; сейчас только int-минуты (`Minutes`/`SetMinutes`).
  Расширяется строковыми ключами, существующий API и ключи takeover.* не
  трогать.
- `auth.Middleware(verifier)` + `auth.RequireRole(auth.RoleAdmin)` (M7/M8) —
  роль admin НЕ наследуется manager'ом, перечислять явно; claims —
  `auth.ClaimsFrom(c)`, `sub` = id менеджера строкой.
- Redis-клиент проекта (go-redis v9) — паттерн счётчиков INCR+ExpireNX
  есть в rate-limit M8; но здесь fail-CLOSED, а не fail-open (§2.2 ТЗ).
- bcrypt — `golang.org/x/crypto/bcrypt` уже в зависимостях (M7, пароли).
- One-off команды — образец `cmd/create-manager` (POSTGRES_DSN, ввод со stdin).
- Конвенция ошибок API — `{"error": "...", "code": "ERR_CODE"}` (CLAUDE.md §5).
- Интеграционные тесты — POSTGRES_TEST_DSN / REDIS_TEST_ADDR, иначе skip;
  БД interfin_test; CI гоняет `go test -p 1`.

## Задачи

1. **Миграции 0016–0020** — ровно по DDL из ТЗ §5:
   `emma_prompt_versions` (+ частичный уникальный индекс is_current),
   `emma_kb_files` (UNIQUE filename), `emma_send_files`, `emma_contacts`,
   `emma_events` (+ два индекса). Down-миграции. GORM-модели в
   `internal/models` (помнить грабли: BOOLEAN с дефолтом — тег `default:`,
   слайсы/JSONB — тег `type:`, см. M13-граблю dialog_mode). schema-lint
   зелёный.

2. **Строковые settings.** В `internal/settings`: `String(ctx, key) string`
   и `SetString(ctx, key, val) error` с тем же кэшем 30с и списком
   допустимых ключей + дефолтов:
   `emma_panel.pin_hash` (""), `emma_panel.prompt_token_limit` ("1200"),
   `emma_panel.welcome_text` (""), `emma_panel.manager_button_enabled` ("false"),
   `emma_panel.manager_button_text` ("Связаться с менеджером"),
   `emma_panel.handoff_confirm_text` ("Сейчас свяжу вас с менеджером, ожидайте"),
   `emma_panel.alert_chat_id` ("").
   `pin_hash` — служебный: НЕ отдаётся через существующий `GET /api/settings`
   и не принимается через `PATCH /api/settings` (только внутренний API).

3. **Пакет `internal/emma` (или `internal/handlers/emma.go` + сервис):
   PIN-логика.**
   - Формат PIN: ровно 6 цифр (`^\d{6}$`), иначе 400.
   - Хэш: bcrypt cost 12 → settings `emma_panel.pin_hash`.
   - Сессия: Redis `emma:pin:<manager_id>` = "1", TTL 30 мин; каждый
     запрос через PIN-middleware продлевает (sliding).
   - Брутфорс: `emma:pin:fail:<manager_id>` INCR + ExpireNX 15 мин;
     ≥5 → verify отвечает 429 `{"code":"PIN_LOCKED","retry_after":<sec>}`
     не проверяя PIN; успешный verify сбрасывает счётчик.
   - **Fail-closed:** любая ошибка Redis в verify/middleware → 503
     `{"code":"PIN_UNAVAILABLE"}` (обоснование в ТЗ §2.2).
   - Тайминг-атаки не наш профиль (bcrypt сам медленный), но сравнение
     статусов «PIN не задан»/«PIN неверен» наружу не различать: оба 401
     `{"code":"PIN_INVALID"}`; «не задан» отдаёт только `pin/status`.

4. **Роуты `/api/emma/pin/*`** (за auth + RequireRole(admin), БЕЗ
   PIN-middleware):
   - `GET /api/emma/pin/status` → `{pin_set: bool, session_active: bool}`;
   - `POST /api/emma/pin/setup` `{pin}` — только если pin_hash пуст,
     иначе 409 `{"code":"PIN_ALREADY_SET"}`; после установки сессия
     открывается сразу (владельцу не вводить PIN дважды);
   - `POST /api/emma/pin/verify` `{pin}` — открыть сессию / 401 / 429;
   - `POST /api/emma/pin/change` `{old_pin, new_pin}` — по старому PIN;
     смена НЕ рвёт активную сессию;
   - `DELETE /api/emma/pin/session` — закрыть сессию (выход из панели).

5. **PIN-middleware** `emma.RequirePIN(redis)` — проверяет и продлевает
   `emma:pin:<sub>`; нет сессии → 401 `{"code":"PIN_REQUIRED"}`.
   Каркас группы: `r.Group("/api/emma", auth.Middleware(v),
   auth.RequireRole(auth.RoleAdmin), emma.RequirePIN(rdb))` — pin/*
   регистрируется отдельной группой без RequirePIN. Контракт для
   EP-02…EP-06: свои ручки вешаются на защищённую группу, ничего про
   PIN не зная.

6. **`GET /api/emma/status`** (за полной цепочкой): `{telegram_token_set,
   anthropic_key_set, model, webhook_url_set}` — только факты «задан/не
   задан» из конфига + имя модели; сами значения не выводить никогда
   (ТЗ §2.3).

7. **`cmd/reset-emma-pin`** — one-off: очищает `emma_panel.pin_hash` и
   убивает PIN-сессии/счётчики в Redis (по образцу create-manager;
   подтверждение y/N со stdin). В Dockerfile добавить бинарь (образец —
   create-manager/index-kb).

8. **Аудит:** slog-запись на setup/verify(успех и провал)/change/reset
   и на каждый мутирующий запрос будущих ручек через middleware группы:
   `manager_id`, метод, путь, timestamp. Без значений PIN в логах
   (CLAUDE.md §5).

## Вне скоупа (не делать в EP-01)
- Ручки промпта/КБ/файлов/контактов/сценариев/статистики — EP-02…EP-06
  (таблицы уже созданы здесь, но код к ним не пишется).
- Маркер-парсер `{{file:N}}`/`{{handoff}}` — EP-04 (решение по ТЗ §8:
  парсер живёт в EP-04, EP-05 зависит от EP-04).
- Фронтенд PIN-экранов — EP-07.
- Поднятие `token_budget.system_prompt` до 5000 — EP-02 (там же правка
  CLAUDE.md).
- nginx `client_max_body_size` и включение `data/emma/` в B2-бэкапы —
  EP-03 (первый эпик с загрузкой файлов).

## Контракт наружу
- Защищённая группа `/api/emma/*` (admin + PIN) — EP-02…EP-06 вешают ручки.
- `settings.String/SetString` + ключи `emma_panel.*` — используют EP-02
  (prompt_token_limit), EP-05 (welcome/button/handoff), EP-06 (alert_chat_id).
- Таблицы 0016–0020 и GORM-модели — весь модуль.
- Коды ошибок для фронта EP-07: `PIN_REQUIRED` (401), `PIN_INVALID` (401),
  `PIN_LOCKED` (429 + retry_after), `PIN_ALREADY_SET` (409),
  `PIN_UNAVAILABLE` (503).
- `GET /api/settings` по-прежнему НЕ отдаёт `emma_panel.pin_hash`.

## Критерии приёмки
- [ ] Все 5 миграций накатываются и откатываются (`migrate up/down`);
      schema-lint зелёный; два лида в emma_events c ON DELETE SET NULL
      ведут себя корректно при erasure (интеграционный тест).
- [ ] manager получает 403 на ЛЮБОЙ `/api/emma/*` (включая pin/status);
      admin без PIN-сессии — 401 `PIN_REQUIRED` на защищённой группе
      (contract-тест по образцу api_contract_test).
- [ ] Bootstrap: pin/status → pin_set=false; setup «123456» → 200 и сессия
      активна; повторный setup → 409; verify неверного → 401.
- [ ] 5 неверных verify подряд → 429 c retry_after; верный PIN во время
      блокировки → тоже 429 (не проверяется); после 15 мин (в тесте —
      подмена TTL) верный PIN проходит (интеграционный тест с Redis,
      REDIS_TEST_ADDR).
- [ ] Сессия: после verify запросы к защищённой группе проходят и продлевают
      TTL; DELETE session → следующий запрос 401 PIN_REQUIRED.
- [ ] Fail-closed: Redis недоступен (тест с закрытым портом/фейком) →
      verify и middleware отвечают 503, НЕ пускают.
- [ ] pin/change: по верному старому — 200, сессия жива; по неверному — 401.
- [ ] `cmd/reset-emma-pin` очищает hash и сессии; после него pin/status →
      pin_set=false.
- [ ] settings: String/SetString работают с кэшем; PATCH /api/settings
      с ключом emma_panel.pin_hash → 400/ERR_UNKNOWN_KEY; GET /api/settings
      не содержит pin_hash; старые ключи takeover.* работают как раньше
      (регресс-тест).
- [ ] `go build ./...`, `go vet ./...` чисто; существующие тесты зелёные
      (`go test -p 1`).
