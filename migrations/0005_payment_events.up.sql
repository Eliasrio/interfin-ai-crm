-- 0005_payment_events (M1): SRS §8.3.
-- НЕ удаляется при LGPD erasure: фискальная retention 5 лет,
-- Receita Federal (AQ²-fix #4, CLAUDE.md §4.8). FK без CASCADE — намеренно.
CREATE TABLE payment_events (
  id              BIGSERIAL PRIMARY KEY,
  lead_id         BIGINT REFERENCES leads(id),
  gateway         VARCHAR(32),
  status          VARCHAR(16),
  amount_due      NUMERIC(20,8),
  amount_received NUMERIC(20,8),
  net_received    NUMERIC(20,8),
  currency        VARCHAR(10),
  tolerance_ok    BOOLEAN,
  raw_payload     JSONB,
  created_at      TIMESTAMPTZ DEFAULT NOW()
);
