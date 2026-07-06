# M6 — Payment Webhook (USDT/USDC)

**Ветка:** `feat/m6-payment`
**Зависимости:** M1, M5
**Разделы SRS:** §3.3 (Политика оплаты USDT), §5.5 (Payment Webhook Security)

## Цель
Приём коллбэков крипто-шлюза, tolerance-логика оплаты, авто-переход стадий по
факту платежа.

## Задачи
1. Эндпоинт `POST /webhook/payment`.
2. **Безопасность** (§5.5):
   - HMAC-SHA256 подпись тела (ключ `payment.hmac_secret`);
   - replay-защита: nonce + timestamp, окно ±5 мин;
   - невалидная подпись → 403.
3. Запись в `payment_events` (все поля §8.3): amount_due, amount_received,
   net_received, tolerance_ok, raw_payload.
4. **Tolerance-логика** (§3.3):
   - `net_received >= amount_due * 0.98` → `tolerance_ok=true` → Stage 3
     (Оплачено: ждут ссылку);
   - иначе → `manual_resolution=true` → Stage 4 (Не оплачено), TTL 48ч;
   - gas fees учтены на стороне шлюза, оперируем `net_received`.
   - `underpaid_tolerance_pct` из конфига (не хардкод).
5. Переход стадии через state machine M5 (Payment переопределяет авто-триггеры).
6. WS-событие `payment_received` (потребитель M9).

## Контракт наружу
- Оплата двигает лид в Kanban автоматически.
- `payment_events` — источник для LGPD export (M8) и фискальной retention.

## Критерии приёмки
- [ ] Невалидная HMAC-подпись → 403; повтор nonce → отклонён (§5.5).
- [ ] USDT 99 из 100 → Stage 3; 97 из 100 → Stage 4 (IQ-3).
- [ ] tolerance_pct берётся из конфига.
- [ ] `payment_events` заполнен корректно, `net_received` учитывает gas fee.
