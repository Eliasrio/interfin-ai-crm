-- Возврат к FK без action (как в 0005).
ALTER TABLE payment_events
  DROP CONSTRAINT payment_events_lead_id_fkey;
ALTER TABLE payment_events
  ADD CONSTRAINT payment_events_lead_id_fkey
    FOREIGN KEY (lead_id) REFERENCES leads(id);
