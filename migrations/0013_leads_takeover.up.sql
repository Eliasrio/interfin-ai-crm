-- 0013 (M13): human takeover — режим диалога лида.
--   dialog_mode        — 'bot' (ведёт Эмма) | 'human' (менеджер забрал диалог);
--   bot_silenced_until — «Эмма молчит до» (автопилот hybrid: пауза после
--                        реплики менеджера при dialog_mode='bot');
--   taken_by           — id менеджера, взявшего диалог (UI и напоминания).
-- LGPD: поля лида не идентифицируют — erasure их не трогает, export отдаёт
-- как есть (решение task M13 §1).
ALTER TABLE leads ADD COLUMN dialog_mode VARCHAR(8) NOT NULL DEFAULT 'bot'
    CHECK (dialog_mode IN ('bot', 'human'));
ALTER TABLE leads ADD COLUMN bot_silenced_until TIMESTAMPTZ;
ALTER TABLE leads ADD COLUMN taken_by BIGINT;
