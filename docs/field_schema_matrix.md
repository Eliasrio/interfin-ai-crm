# Field ↔ Schema матрица (SRS §8.5, AQ²-fix #1)

Living reference: какие «особые» поля где живут и кто их использует.
Машинная проверка соответствия модели↔миграции — `go run ./cmd/schema-lint`
(CI, каждый PR). Эта таблица — человекочитаемое дополнение, не замена.

| Поле | Таблица | Миграция | Go-модель | Используется в |
|---|---|---|---|---|
| `pending_task` | `leads` | `0003_leads` | `models.Lead.PendingTask` | §11.2 Graceful Degradation (Redis down → TRUE, M2) |
| `escalated_at` | `leads` | `0003_leads` | `models.Lead.EscalatedAt` | §3.5 Anti-Spam TTL (эскалация 48ч, M5) |
| `ttl_task_id` | `leads` | `0003_leads` | `models.Lead.TTLTaskID` | §3.4, §6.4 TTL (reset = DeleteTask + enqueue, M5) |
| `anti_spam_count` | `leads` | `0003_leads` | `models.Lead.AntiSpamCount` | §3.2, §3.5 (per-stage inbound, сброс при смене стадии, M5) |
| `tolerance_ok` | `payment_events` | `0005_payment_events` | `models.PaymentEvent.ToleranceOk` | §3.3 USDT (underpaid ≤ 2%, M6) |
| `manual_resolution` | `leads` | `0003_leads` | `models.Lead.ManualResolution` | §3.3 (недоплата сверх tolerance → ручной разбор, M6) |
| `consent_given_at` | `leads` | `0003_leads` | `models.Lead.ConsentGivenAt` | §9 LGPD (согласие при первом inbound, M2) |

Смежные инварианты (CLAUDE.md §4):

- `message_count` — только inbound; инкремент зашит в
  `repo.MessageRepo.CreateInbound`, outbound счётчики не трогает (§4.3).
- TTL считается от `last_activity_at` (обновляется там же), не от
  `created_at` (§4.7).
- `payment_events` не удаляется при LGPD erasure — фискальная retention
  5 лет; `telegram_user_id` при erasure хешируется, не NULL (§4.8).
- Embeddings-таблица (`vector(1024)`) появляется в M4 вместе с HNSW-индексом;
  M1 включает только расширение pgvector (`0002_pgvector`).
