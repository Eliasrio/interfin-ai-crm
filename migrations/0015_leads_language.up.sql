-- 0015 (M14): язык клиента — ru / en / es (решение владельца, NEXT_STEPS 2.1).
--   NULL = «ещё не определён»: лид не прислал ни одного текстового сообщения;
--   выставляется ОДИН раз детекцией первого текстового inbound (ingestion),
--   дальше меняется только руками менеджера (PATCH /api/leads/:id/language).
-- LGPD: язык не идентифицирует человека — erasure не трогает, export отдаёт
-- как есть (решение task M14 §1).
ALTER TABLE leads ADD COLUMN language VARCHAR(2)
    CHECK (language IN ('ru', 'en', 'es'));
