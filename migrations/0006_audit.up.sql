-- 0006_audit (M1): SRS §8.4 — rag_audit и lgpd_audit.
-- lead_id без FK: аудит обязан переживать удаление лида
-- (retention-cron §9.1 удаляет leads старше 90 дней после erasure).
CREATE TABLE rag_audit (
  id         BIGSERIAL PRIMARY KEY,
  lead_id    BIGINT,
  query_text TEXT,
  results    INT,
  threshold  FLOAT,
  created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE lgpd_audit (
  id           BIGSERIAL PRIMARY KEY,
  action       VARCHAR(32),
  lead_id      BIGINT,
  performed_by VARCHAR(64),
  ip_address   INET,
  created_at   TIMESTAMPTZ DEFAULT NOW()
);
