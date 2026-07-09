-- 0014 (M13): settings — key/value настройки CRM, редактируемые из UI.
-- Фундамент «полного управления Эммой» (NEXT_STEPS блок 3); в M13 живут три
-- ключа контура takeover: takeover.hybrid_pause_minutes (дефолт 30),
-- takeover.reminder_minutes (10), takeover.pickup_minutes (10).
-- Отсутствие строки = дефолт из кода (internal/settings) — таблица хранит
-- только переопределения, поэтому без сидов.
CREATE TABLE settings (
    key        VARCHAR(64) PRIMARY KEY,
    value      TEXT        NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
