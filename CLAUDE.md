# CLAUDE.md — Interfin AI-CRM

> Единый свод правил для всех сессий Claude Code по этому проекту.
> Читается автоматически в начале каждой сессии. Держи открытым.

---

## 1. Что это за проект

**Interfin AI-CRM (Telegram Deal Room)** — CRM для квалификации русскоязычных лидов
через Telegram-бот с AI. Компания: INTERFIN GROUP LTDA (Búzios, RJ, Brazil).
Compliance: **LGPD**. Полное ТЗ — в `SRS_v2.3.docx`.

Три ключевые способности:
- Telegram-бот на Claude автоматически ведёт диалог с лидом (RAG-контекст).
- Менеджер видит real-time Kanban-доску (React + WebSocket).
- Оплата USDT/USDC двигает лид по стадиям автоматически.

---

## 2. Как устроена разработка

Проект разбит на **12 эпиков (M0–M11)**. Каждый эпик = отдельная сессия +
отдельная git-ветка + отдельный task-файл в `tasks/`.

**Правило одной сессии:** в одну сессию Claude Code берётся ОДИН task-файл.
Не смешивай эпики. Закончил M2 — закоммить, начни новую сессию для M3.

**Порядок и зависимости** (не нарушать):

```
M0 Скелет ──▶ M1 Данные ──┬──▶ M2 Ingestion ──▶ M3 Воркер+Claude ──▶ M4 RAG
                          │                              │
                          │                              ▼
                          │                        M5 Kanban SM ──▶ M6 Payment
                          │
                          └──▶ M7 Auth ──▶ M8 REST API ──▶ M9 WebSocket ──▶ M10 React
                                                                              │
                                       M11 HA + Monitoring ◀──────────────────┘
```

- **M0 и M1 — строго первыми.** Без них ничего не собрать.
- **M7 (Auth)** можно вести параллельно ветке M2→M3→M4.
- **M10 (React)** нельзя начать, пока не готовы M8 (API) и M9 (WebSocket) — это ок.
- **M11 — последним**, оборачивает готовую систему.

---

## 3. Технологический стек (не менять без причины)

| Слой | Технология |
|---|---|
| Backend | Go 1.22 + Gin |
| Telegram | gopkg.in/telebot.v3 (webhook, НЕ polling) |
| БД | PostgreSQL 16 + GORM + pgx (**SimpleProtocol**) |
| Vector | pgvector 0.7, `vector(1024)` |
| Embeddings | **Voyage AI** voyage-3 (НЕ OpenAI) |
| Очереди | Redis 7 + Asynq v0.24 |
| Миграции | golang-migrate v4 |
| AI | Claude Sonnet 5 (`claude-sonnet-5`) — замена: SRS-модель `claude-3-5-sonnet-20241022` отключена Anthropic 28.10.2025, API отдаёт 404 (зафиксировано в M3). В клиенте thinking выключен явно — иначе на Sonnet 5 он включён по умолчанию и ест бюджет ответа §7.2 |
| Frontend | React 18 + gorilla/websocket |
| Auth | JWT RS256 + Refresh Token |
| Deploy | Docker Compose (dev) / Swarm (prod) |
| Monitoring | Prometheus + Grafana + Alertmanager |

---

## 4. Жёсткие правила (нарушение = баг)

Эти вещи мы уже проходили в review. Не повторяй.

1. **Single source of truth для схемы.** Любое поле, которое ты упоминаешь в коде
   или комментарии, ОБЯЗАНО существовать в миграции `migrations/`. Перед PR —
   прогони schema-lint (см. M1). Прецедент: `pending_task` однажды забыли добавить.

2. **pgx только в SimpleProtocol.** `cfg.DefaultQueryExecMode =
   pgx.QueryExecModeSimpleProtocol` + GORM `PrepareStmt: false`. Иначе pgbouncer
   в transaction mode ломает prepared statements под нагрузкой.

3. **Счётчик message_count — только inbound.** `direction='inbound'`. Ответы бота
   не считаются.

4. **Telegram webhook отвечает 200 НЕМЕДЛЕННО**, до вызова Claude. Всё тяжёлое —
   в Asynq-воркер. Таймаут Telegram = 5 сек, latency Claude = 3–15 сек.

5. **Asynq не дедуплицирует сам.** Всегда `asynq.TaskID(hash) + asynq.Unique()`.

6. **Токены Claude считаем гибридно.** Локальная оценка `len/4`; точный
   `count_tokens` вызываем только когда оценка > 7500. Никакого tiktoken.

7. **TTL считаем от `last_activity_at`**, не от `created_at`. Reset —
   `inspector.DeleteTask` + новый enqueue.

8. **LGPD erasure НЕ трогает `payment_events`** (фискальная retention 5 лет).
   `telegram_user_id` при erasure ХЕШИРУЕТСЯ (не NULL — оно NOT NULL).
   UNIQUE на нём — только по живым строкам (частичный индекс, 0011):
   хеш детерминированный, повторный цикл create→erase одного человека
   легально даёт две стёртые строки с одним хешом.

9. **Секреты только через env / Docker secrets.** Никаких ключей в коде, конфигах,
   коммитах. Config читается через viper с `${ENV_VAR}` подстановкой.

10. **Никакого polling у Telegram в prod.** Только webhook через `gin.WrapH`.

---

## 5. Соглашения по коду

- **Структура:** `cmd/` (точки входа), `internal/` (бизнес-логика: handlers,
  workers, services, models, repo), `migrations/`, `config/`, `web/` (React).
- **Ошибки:** оборачивай через `fmt.Errorf("...: %w", err)`. Не глотай ошибки.
- **API-ошибки клиенту:** `{"error": "...", "code": "ERR_CODE"}` + HTTP status.
- **Логи:** структурные (slog), с `lead_id` где применимо. Без секретов в логах.
- **Тесты:** каждый эпик закрывается зелёными тестами по своим критериям приёмки.
  Метод указан в task-файле (unit / integration / e2e / load).
- **Коммиты:** `M<N>: <что сделано>`. Одна логическая единица — один коммит.

---

## 6. Definition of Done для любого эпика

Эпик считается закрытым, только когда:

- [ ] Все пункты «Задачи» из task-файла реализованы.
- [ ] Критерии приёмки эпика проходят (тесты зелёные).
- [ ] Ничего из «Жёстких правил» (§4) не нарушено.
- [ ] `go build ./...` и `go vet ./...` без ошибок.
- [ ] Для эпиков с БД — schema-lint зелёный.
- [ ] Изменения закоммичены в ветку эпика.

---

## 7. Как пользоваться task-файлами

Каждый файл в `tasks/M<N>_*.md` содержит: цель, зависимости, релевантные
разделы SRS, конкретные задачи, контракты (что этот эпик отдаёт наружу),
критерии приёмки. Открывай task-файл текущего эпика + этот CLAUDE.md.
Не грузи всё ТЗ разом — только нужные разделы, они указаны в каждом файле.
