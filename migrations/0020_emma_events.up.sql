-- 0020 (EP-01): emma_events — журнал работы Эммы для вкладки «Статистика»
-- (ТЗ §5): ответы (с токенами usage), отправки файлов, handoff'ы, ошибки.
-- LGPD: персональных данных нет, lead_id с ON DELETE SET NULL — retention
-- физически удаляет лида, событие остаётся анонимным.
CREATE TABLE emma_events (
  id               BIGSERIAL PRIMARY KEY,
  event_type       TEXT NOT NULL
                   CHECK (event_type IN ('reply','file_sent','handoff','error')),
  error_kind       TEXT,       -- llm_api/telegram_api/timeout/file_not_found/kb_index
  detail           TEXT,
  lead_id          BIGINT REFERENCES leads(id) ON DELETE SET NULL,
  send_file_id     BIGINT REFERENCES emma_send_files(id) ON DELETE SET NULL,
  response_time_ms INTEGER,
  tokens_in        INTEGER,    -- usage.input_tokens ответа Claude (event_type='reply')
  tokens_out       INTEGER,    -- usage.output_tokens
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX emma_events_type_time_idx ON emma_events (event_type, created_at);
CREATE INDEX emma_events_time_idx ON emma_events (created_at);
