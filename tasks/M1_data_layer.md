# M1 — Слой данных

**Ветка:** `feat/m1-data`
**Зависимости:** M0
**Разделы SRS:** §8 целиком (8.1 leads, 8.2 messages, 8.3 payment_events,
8.4 rag_audit/lgpd_audit, 8.5 field↔schema матрица, 8.6 migrations)

## Цель
Все таблицы, GORM-модели, pgx SimpleProtocol, schema-lint в CI. Это фундамент
данных для всей системы.

## Задачи
1. Миграции golang-migrate для ВСЕХ пяти таблиц из §8. Копируй CREATE TABLE
   дословно из SRS — все поля, индексы, CHECK-констрейнты, `vector(1024)` в
   embeddings-таблице.
   - `leads` (§8.1) — включая `pending_task`, `escalated_at`, `ttl_task_id`,
     `anti_spam_count`, `consent_given_at`, `deleted_at`. Все индексы.
   - `messages` (§8.2) — с `ON DELETE CASCADE` и CHECK на direction.
   - `payment_events` (§8.3).
   - `rag_audit`, `lgpd_audit` (§8.4).
2. GORM-модели, точно отражающие схему. Теги полей соответствуют колонкам.
3. **pgx SimpleProtocol** (жёсткое правило §4.2 CLAUDE.md):
   `cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol` +
   GORM `&gorm.Config{PrepareStmt: false}`.
4. Repository-слой (интерфейсы + реализация) для leads/messages/payments.
5. **schema-lint скрипт для CI:** парсит миграции, извлекает список колонок,
   сверяет со списком полей, используемых в Go-моделях. Падает, если модель
   ссылается на несуществующую колонку. Это защита от рецидива AQ²-1.
6. Field↔schema матрица (§8.5) как комментарий/док в репо — living reference.

## Контракт наружу
- Repository-интерфейсы: `LeadRepo`, `MessageRepo`, `PaymentRepo`.
- Готовая схема БД, применяемая `migrate up`.
- schema-lint, запускаемый в CI на каждом PR.

## Критерии приёмки
- [ ] `migrate up` и `migrate down` на чистой БД без ошибок (AQ²-9).
- [ ] Каждое поле, используемое в Go-коде, есть в CREATE TABLE (AQ²-1 schema-lint).
- [ ] pgx SimpleProtocol активен; под нагрузкой 0 ошибок "prepared statement
      does not exist" (AQ²-3, проверяется полноценно в M11).
- [ ] Все индексы из §8.1 созданы.
