-- 0016 (EP-01): emma_prompt_versions — версии системного промпта Эммы
-- (ТЗ EMMA_PANEL_TZ_v2 §5). История неограничена, активная версия ровно
-- одна — частичный уникальный индекс по is_current.
CREATE TABLE emma_prompt_versions (
  id            BIGSERIAL PRIMARY KEY,
  system_prompt TEXT NOT NULL,
  forbidden_topics JSONB NOT NULL DEFAULT '[]',   -- массив строк
  style         TEXT NOT NULL DEFAULT 'neutral'
                CHECK (style IN ('formal','friendly','neutral','expert')),
  is_current    BOOLEAN NOT NULL DEFAULT FALSE,
  created_by    BIGINT REFERENCES managers(id) ON DELETE SET NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX emma_prompt_current_key
  ON emma_prompt_versions (is_current) WHERE is_current;  -- одна активная
