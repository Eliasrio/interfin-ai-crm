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

## Про критерии приёмки

Метки `IQ-N` / `AQ²-N` в критериях ссылаются на review-историю ТЗ (баги,
которые уже ловили). Прохождение этих тестов = защита от регресса.
Полная таблица критериев — в §16 SRS.
