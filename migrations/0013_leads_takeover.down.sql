-- 0013 down: откат полей режима диалога (M13).
ALTER TABLE leads DROP COLUMN IF EXISTS taken_by;
ALTER TABLE leads DROP COLUMN IF EXISTS bot_silenced_until;
ALTER TABLE leads DROP COLUMN IF EXISTS dialog_mode;
