-- 0017 (EP-01): emma_kb_files — файлы базы знаний панели Эммы (ТЗ §5).
-- Файл лежит в data/emma/kb/<uuid>, здесь — оригинальное имя и статус
-- RAG-индексации; UNIQUE(filename): повторная загрузка = замена.
CREATE TABLE emma_kb_files (
  id           BIGSERIAL PRIMARY KEY,
  filename     TEXT NOT NULL,                  -- оригинальное имя
  mime_type    TEXT NOT NULL,
  file_path    TEXT NOT NULL,                  -- data/emma/kb/<uuid>
  file_size    BIGINT NOT NULL,
  index_status TEXT NOT NULL DEFAULT 'pending'
               CHECK (index_status IN ('pending','indexed','error')),
  index_error  TEXT,
  chunks_count INTEGER NOT NULL DEFAULT 0,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (filename)                            -- повторная загрузка = замена
);
