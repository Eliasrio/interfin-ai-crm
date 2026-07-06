-- 0002_pgvector (M1): расширение pgvector 0.7 (SRS §5, vector(1024)).
-- Сама embeddings-таблица (база знаний RAG) создаётся в M4 вместе с
-- HNSW-индексом (§7.1) — в §8 её CREATE TABLE нет, копировать нечего.
-- Здесь только включаем расширение, чтобы тип vector был доступен.
CREATE EXTENSION IF NOT EXISTS vector;
