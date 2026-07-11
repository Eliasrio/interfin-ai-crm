-- 0015 down: откат языка клиента (M14).
ALTER TABLE leads DROP COLUMN IF EXISTS language;
