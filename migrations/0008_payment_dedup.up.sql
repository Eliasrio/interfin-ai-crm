-- 0008 (M6): дедупликация платёжных вебхуков — вторая линия поверх
-- replay-защиты §5.5. Nonce живёт в Redis (payment:nonce:<update_id>,
-- TTL 2× окна) и может потеряться (flush/failover) или быть возвращён
-- после провала обработки; уникальный индекс гарантирует, что повторная
-- вставка того же update_id шлюза станет no-op (ON CONFLICT DO NOTHING
-- в repo.PaymentRepo.Create), а не второй фискальной записью.
-- Строки без gateway/update_id (NULL в выражении) индекс не конфликтует.
CREATE UNIQUE INDEX idx_payment_events_dedup
  ON payment_events (gateway, (raw_payload->>'update_id'));
