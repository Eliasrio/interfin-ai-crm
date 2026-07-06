-- 0003_leads (M1): SRS §8.1 — дословно, включая AQ²-fix поля
-- pending_task (#1), escalated_at (#8), ttl_task_id, anti_spam_count,
-- consent_given_at, deleted_at.
CREATE TABLE leads (
  id                BIGSERIAL PRIMARY KEY,
  telegram_user_id  BIGINT        NOT NULL UNIQUE,
  name              VARCHAR(255),
  phone             VARCHAR(32),
  tg_username       VARCHAR(128),
  stage_id          SMALLINT      NOT NULL DEFAULT 1,
  message_count     INT           NOT NULL DEFAULT 0,   -- только inbound (CLAUDE.md §4.3)
  anti_spam_count   INT           NOT NULL DEFAULT 0,   -- per-stage inbound
  last_activity_at  TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
  manual_resolution BOOLEAN       NOT NULL DEFAULT FALSE,
  ttl_task_id       VARCHAR(64),
  pending_task      BOOLEAN       NOT NULL DEFAULT FALSE, -- AQ²-fix #1
  escalated_at      TIMESTAMPTZ,                          -- AQ²-fix #8
  consent_given_at  TIMESTAMPTZ,
  deleted_at        TIMESTAMPTZ,
  created_at        TIMESTAMPTZ   NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_leads_stage ON leads(stage_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_leads_activity ON leads(last_activity_at);
CREATE INDEX idx_leads_pending ON leads(pending_task) WHERE pending_task = TRUE;
