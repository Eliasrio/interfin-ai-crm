-- 0012 (M12): messages.author — кто написал outbound: 'bot' | 'manager:<id>'.
-- NULL в старых строках читается как bot (историю не переписываем);
-- у inbound автор всегда NULL — автор и так лид (direction='inbound').
-- Нужна чату менеджера в UI («кто написал») и прозрачности LGPD-экспорта.
-- Erasure не трогает: author лида не идентифицирует (обнуляется content).
ALTER TABLE messages ADD COLUMN author VARCHAR(32);
