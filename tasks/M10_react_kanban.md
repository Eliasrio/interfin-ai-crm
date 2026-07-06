# M10 — React Kanban Frontend

**Ветка:** `feat/m10-frontend`
**Зависимости:** M8 (REST API), M9 (WebSocket)
**Разделы SRS:** §10 (клиентская часть), §4 (API-контракт), §4.3 (WS events)

## Цель
React-доска Kanban для менеджера: 8 колонок стадий, живое обновление карточек
через WebSocket, ручные переходы, LGPD-действия.

## Задачи
1. React 18 приложение. Аутентификация: login-форма → `POST /auth/login`,
   хранение access token в памяти, refresh через cookie.
2. **Kanban-доска**: 8 колонок по стадиям (§3.1). Карточки лидов, drag-and-drop
   для ручной смены стадии → `PATCH /api/leads/:id/stage`.
3. **WebSocket-клиент** (§10):
   - подключение к `/ws/kanban` с `Sec-WebSocket-Protocol: Bearer.<token>`;
   - обработка событий `stage_change`, `antispam_alert`, `payment_received`,
     `ttl_warning` → обновление карточек без перезагрузки;
   - **catch-up при реконнекте** (§10.3): при обрыве запомнить `last_event_ts`,
     после refresh токена вызвать `GET /api/leads?updated_since` перед resubscribe;
   - fallback в polling при недоступности WS.
4. Карточка лида: история диалога, payment-статус, кнопки ручных переходов
   (Stage 5/6/7), индикаторы TTL и anti-spam/эскалации.
5. **LGPD-панель** (admin/manager): кнопки erasure и export
   (`DELETE .../erase`, `GET .../export`).
6. Обработка ошибок API по формату `{"error","code"}`; 401 → тихий refresh
   или редирект на login.

## Контракт наружу
- Готовый UI для менеджера — конечный пользовательский интерфейс системы.

## Критерии приёмки
- [ ] Смена стадии на бэке → карточка двигается в UI < 500 мс (IQ-10 e2e).
- [ ] Реконнект WS не теряет события (catch-up работает, AQ²-5 e2e).
- [ ] Ручной drag-and-drop корректно вызывает PATCH и переставляет карточку.
- [ ] LGPD erasure/export доступны только соответствующим ролям.
