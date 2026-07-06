-- 0011 (фикс бага M8, найден на живом стенде M10): повторный цикл erasure
-- одного человека. Хеш telegram_user_id детерминирован (§9.3): вернувшийся
-- после erasure лид создаётся заново с тем же telegram_user_id, и его
-- повторный erase пишет ТОТ ЖЕ хеш, что уже лежит в старой стёртой строке, —
-- глобальный UNIQUE из 0003 падал с 23505, DELETE /api/lgpd/leads/:id/erase
-- отвечал 500. Бизнес-смысл уникальности — дедуп ЖИВЫХ лидов в ingestion;
-- для стёртых строк одинаковый хеш корректен (один титуляр, стёртый дважды),
-- обе строки доживают до retention §9.1. Поэтому UNIQUE сужается до
-- частичного индекса по активным строкам. Детерминированность хеша и
-- семантика LGPD_SALT не меняются; NOT NULL остаётся (CLAUDE.md §4.8).
ALTER TABLE leads DROP CONSTRAINT leads_telegram_user_id_key;
CREATE UNIQUE INDEX leads_telegram_user_id_active_key
  ON leads (telegram_user_id) WHERE deleted_at IS NULL;
