# M9 — WebSocket Real-Time Push

**Ветка:** `feat/m9-websocket`
**Зависимости:** M5, M7, M8
**Разделы SRS:** §10 (10.1 архитектура, 10.2 heartbeat, 10.3 catch-up),
§4.3 (WS events schema)

## Цель
Real-time доставка событий (смена стадии, оплата, anti-spam) на React-фронт
через WebSocket. С catch-up при реконнекте и корректным heartbeat.

## Задачи
1. **WS Hub** (goroutine, §10.1): `SUBSCRIBE crm:events` (Redis pub/sub) →
   broadcast всем авторизованным клиентам через gorilla/websocket.
2. `GET /ws/kanban` с upgrade. **Auth** (§5.3, helper из M7): JWT из
   `Sec-WebSocket-Protocol: Bearer.<token>`; истёкший → close code 4001.
3. **События** (§4.3): `stage_change`, `antispam_alert`, `payment_received`,
   `ttl_warning`. Сериализация по схеме из SRS. Издатели — M5, M6.
4. **Heartbeat — ТОЛЬКО protocol-level** (§10.2, AQ²-11, жёсткое правило):
   - server ping каждые 30с (`WriteControl(PingMessage)`);
   - `SetPongHandler` продлевает read deadline на 60с;
   - НЕ добавлять app-level `{type:"ping"}` — это дублирование, убрано в v2.3.
5. **Catch-up при реконнекте** (§10.3, AQ²-5):
   - при обрыве Redis pub/sub → клиентам сигнал перейти в polling
     (`GET /api/leads/:id/stage` каждые 5с);
   - при реконнекте WS клиент вызывает
     `GET /api/leads?updated_since={last_event_ts}` (эндпоинт из M8) →
     досинхронизирует пропущенные события до resubscribe.
6. Redis pub/sub через тот же Sentinel pool (готовится в M11, dev — single Redis).

## Контракт наружу
- Живой поток событий для React Kanban (M10).
- Протокол реконнекта с гарантией отсутствия потерь.

## Критерии приёмки
- [ ] Смена стадии в воркере → обновление у клиента < 500 мс (IQ-10).
- [ ] WS reconnect после expiry → catch-up досинхронизирует пропущенное (AQ²-5).
- [ ] Один heartbeat-механизм; idle-соединение закрывается корректно (AQ²-11).
- [ ] Истёкший JWT при upgrade → close 4001.
