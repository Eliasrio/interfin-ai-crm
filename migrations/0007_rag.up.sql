-- 0007_rag (M4): база знаний RAG (SRS §7.1) и сводки диалогов (§7.3).
-- В §8 CREATE TABLE для них нет (решение M1) — схема задаётся здесь.

-- Чанки базы знаний: документ source разбит на куски chunk_index,
-- каждый с Voyage-эмбеддингом voyage-3 (1024 dims, AQ²-fix #2).
CREATE TABLE knowledge_chunks (
  id          BIGSERIAL     PRIMARY KEY,
  source      VARCHAR(255)  NOT NULL,           -- имя документа (файла) базы знаний
  chunk_index INT           NOT NULL,           -- порядковый номер чанка внутри документа
  content     TEXT          NOT NULL,
  embedding   vector(1024)  NOT NULL,
  created_at  TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
  UNIQUE (source, chunk_index)                  -- переиндексация документа = replace, не дубли
);

-- HNSW §7.1: ef_construction=200, m=16; cosine (поиск идёт оператором <=>).
-- ef_search=100 — параметр времени запроса: repo задаёт его через
-- SET LOCAL hnsw.ef_search в транзакции поиска.
CREATE INDEX idx_knowledge_chunks_embedding
  ON knowledge_chunks USING hnsw (embedding vector_cosine_ops)
  WITH (m = 16, ef_construction = 200);

-- Сводка диалога (§7.3): одна на лида, перезаписывается каждые 15 inbound.
-- message_count — счётчик лида на момент генерации: защита от записи
-- устаревшей сводки поверх более свежей.
CREATE TABLE conversation_summaries (
  lead_id       BIGINT      PRIMARY KEY REFERENCES leads(id) ON DELETE CASCADE,
  content       TEXT        NOT NULL,
  message_count INT         NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
