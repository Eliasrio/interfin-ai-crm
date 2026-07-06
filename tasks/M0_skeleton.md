# M0 — Скелет проекта

**Ветка:** `feat/m0-skeleton`
**Зависимости:** нет (первый эпик)
**Разделы SRS:** §12 (Config), §13 (Environments/Deploy), §8.6 (Migrations)

## Цель
«Оно загружается». Репозиторий, конфиг, локальное окружение, миграционный
инструмент, health-эндпоинты. Никакой бизнес-логики.

## Задачи
1. Инициализировать Go-модуль, структуру каталогов: `cmd/`, `internal/`,
   `migrations/`, `config/`.
2. Config через viper: чтение `config.yaml` с подстановкой `${ENV_VAR}`.
   Секции — как в §12 SRS (telegram, database, redis, auth, claude, embeddings,
   aws, kanban, rag, lgpd, monitoring).
3. `docker-compose.yml` (dev): postgres (`pgvector/pgvector:pg16`), redis:7-alpine,
   minio. Порты как в §13.2.
4. Подключить golang-migrate v4. Пустая миграция `0001_init.up.sql` / `.down.sql`.
5. Gin-сервер с эндпоинтами `GET /health` (liveness) и `GET /ready` (readiness,
   проверяет коннект к postgres+redis).
6. `.env.example` со всеми переменными (без реальных значений).
7. Dockerfile с HEALTHCHECK на `/health`.

## Контракт наружу (что дают следующие эпики)
- Рабочий `config`-объект, доступный всем модулям.
- `docker-compose up` поднимает всё окружение за < 60 сек.
- Паттерн миграций для M1.

## Критерии приёмки
- [ ] `docker-compose up` → все сервисы healthy < 60 сек (AQ²-9 smoke test).
- [ ] `GET /health` → 200. `GET /ready` при живых БД+Redis → 200, иначе 503.
- [ ] Config корректно подставляет env-переменные; отсутствие обязательной →
      явная ошибка на старте.
- [ ] `go build ./...` без ошибок.
