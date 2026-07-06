# M5 — Kanban State Machine

**Ветка:** `feat/m5-kanban`
**Зависимости:** M1, M3 (TTL-helper)
**Разделы SRS:** §3 целиком (3.1 стадии, 3.2 счётчик, 3.4 TTL, 3.5 anti-spam)

## Цель
Логика переходов лида по 8 стадиям: авто-триггеры по счётчику, anti-spam с TTL,
ручные переходы менеджера. Сердце бизнес-логики.

## Задачи
1. State machine на 8 стадий (§3.1). Таблица переходов, валидация допустимых
   переходов. **Приоритет:** Manual Manager и Payment Webhook ВСЕГДА
   переопределяют авто-триггеры.
2. Авто-переходы по счётчику:
   - Stage 1 → 2 при `message_count >= 6` (только inbound, §3.2).
3. **TTL-переходы** (§3.4, через helper из M3):
   - Stage 4: 48ч от `last_activity_at` → Stage 8;
   - Stage 6: 5 дней от `last_activity_at` → Stage 8;
   - ручное обновление менеджером сбрасывает TTL (DeleteTask + новый enqueue).
4. **Anti-spam + TTL** (§3.5, AQ²-8):
   - лимит 25 inbound/стадию (`anti_spam_count`) → бот замолкает;
   - WS-событие `antispam_alert` (интеграция с M9);
   - Asynq delayed `antispam:followup` (24ч) → одно follow-up-сообщение;
   - Asynq delayed `antispam:escalate` (48ч) → `manager_escalation` +
     запись `leads.escalated_at`;
   - сброс `anti_spam_count` и отмена задач при переходе стадии.
5. При каждой смене стадии: `PUBLISH crm:events` (payload из §4.3; потребитель — M9).

## Контракт наружу
- `crm:events` publish при каждом переходе — вход для M9 WebSocket.
- API state machine для ручных переходов — используется M8 (PATCH stage).

## Критерии приёмки
- [ ] `message_count` считает только inbound (IQ-2).
- [ ] TTL Stage 6 сбрасывается при ручном обновлении менеджером (IQ-4).
- [ ] Anti-spam: 25 inbound → бот молчит + push (IQ-9); 24ч → follow-up;
      48ч → эскалация (AQ²-8).
- [ ] Manual/Payment переопределяют авто-триггеры.
- [ ] Лид не может застрять навсегда в anti-spam-молчании.
