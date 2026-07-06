-- 0010 (M8): retention-cron §9.1 спустя 90 дней после erasure удаляет
-- строку leads ФИЗИЧЕСКИ, а payment_events обязаны пережить это удаление —
-- фискальная retention 5 лет (Receita Federal, §9.3, AQ²-fix #4,
-- CLAUDE.md §4.8). Исходный FK из 0005 был без action (RESTRICT):
-- DELETE лида с платежами падал бы. ON DELETE SET NULL: фискальная запись
-- остаётся целиком (суммы, raw_payload), обнуляется только привязка к лиду.
ALTER TABLE payment_events
  DROP CONSTRAINT payment_events_lead_id_fkey;
ALTER TABLE payment_events
  ADD CONSTRAINT payment_events_lead_id_fkey
    FOREIGN KEY (lead_id) REFERENCES leads(id) ON DELETE SET NULL;
