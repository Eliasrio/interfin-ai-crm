-- 0004_messages (M1): SRS §8.2 — ON DELETE CASCADE и CHECK на direction.
CREATE TABLE messages (
  id         BIGSERIAL PRIMARY KEY,
  lead_id    BIGINT REFERENCES leads(id) ON DELETE CASCADE,
  direction  VARCHAR(8) CHECK (direction IN ('inbound','outbound')),
  content    TEXT NOT NULL,
  tokens     INT,
  created_at TIMESTAMPTZ DEFAULT NOW()
);
