-- 0019 (EP-01): emma_contacts — справочник контактов и ссылок для секции
-- промпта Эммы (ТЗ §5). sort_order — порядок в промпте и в панели.
CREATE TABLE emma_contacts (
  id         BIGSERIAL PRIMARY KEY,
  type       TEXT NOT NULL
             CHECK (type IN ('phone','whatsapp','telegram','email','website','other')),
  name       TEXT NOT NULL,
  value      TEXT NOT NULL,
  comment    TEXT,
  is_active  BOOLEAN NOT NULL DEFAULT TRUE,
  sort_order INTEGER NOT NULL DEFAULT 0
);
