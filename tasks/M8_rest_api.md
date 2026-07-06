# M8 — REST API

**Ветка:** `feat/m8-api`
**Зависимости:** M1, M5, M7
**Разделы SRS:** §4 (REST API), §9 (LGPD), §4.2 (коды ошибок)

## Цель
Полный REST API для менеджера: список/карточка лидов, ручная смена стадии,
catch-up, LGPD erasure/export. Всё за auth-гейтом из M7.

## Задачи
1. Эндпоинты из таблицы §4.1 (кроме webhook/health/metrics — они в других эпиках):
   - `GET /api/leads` — список с пагинацией; **параметр `?updated_since=ts`**
     для catch-up (§10.3, критично для M9);
   - `GET /api/leads/:id` — карточка;
   - `PATCH /api/leads/:id/stage` — ручная смена стадии (через state machine M5);
   - `GET /api/leads/:id/stage` — текущая стадия (polling fallback).
2. Все под auth middleware (M7), с ролевым гейтом (§5.2).
3. **Формат ошибок** (§4.2): `{"error": "...", "code": "ERR_CODE"}` + HTTP status.
   Коды: 400/401/403/404/409/429/500. Rate limit 100 req/min per IP → 429.
4. **LGPD endpoints** (§9):
   - `DELETE /api/lgpd/leads/:id/erase`:
     - soft delete (`deleted_at=NOW()`);
     - `name/phone/tg_username = NULL`;
     - `messages.content = '[DELETED]'`;
     - **`telegram_user_id` → hash(user_id+salt)**, НЕ NULL (§9.3, AQ²-4);
     - `payment_events` НЕ трогать (фискальная retention, §9.3);
     - запись в `lgpd_audit`.
   - `GET /api/lgpd/leads/:id/export` → JSON: messages, payment_events,
     lgpd_audit. В ответе — пометка о сохранении финансовых записей.
5. LGPD retention cron: удаление leads с `deleted_at > 90 дней`.

## Контракт наружу
- `GET /api/leads?updated_since` — механизм catch-up для M9 WebSocket.
- Полный API для React-фронта (M10).

## Критерии приёмки
- [ ] Все эндпоинты возвращают корректные HTTP статусы (AQ²-1 contract test).
- [ ] Erasure: name/phone/tg_username=NULL, telegram_user_id хеширован,
      payment_events сохранён (AQ²-4).
- [ ] `?updated_since` возвращает лиды, изменённые после ts (быстро, по индексу).
- [ ] Rate limit срабатывает на 101-м запросе за минуту.
