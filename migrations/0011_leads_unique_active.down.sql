-- Возврат к глобальному UNIQUE из 0003. Упадёт, если стёртые строки уже
-- держат одинаковый хеш (тот самый повторный erasure): сперва удалить
-- дубли ретеншеном (DeleteErasedBefore) или вручную.
DROP INDEX leads_telegram_user_id_active_key;
ALTER TABLE leads
  ADD CONSTRAINT leads_telegram_user_id_key UNIQUE (telegram_user_id);
