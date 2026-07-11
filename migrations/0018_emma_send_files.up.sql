-- 0018 (EP-01): emma_send_files — библиотека файлов, которые Эмма отправляет
-- клиентам (ТЗ §5). description — подсказка Эмме, когда файл уместен.
CREATE TABLE emma_send_files (
  id          BIGSERIAL PRIMARY KEY,
  name        TEXT NOT NULL,
  description TEXT NOT NULL,                   -- подсказка Эмме
  file_path   TEXT NOT NULL,
  mime_type   TEXT NOT NULL,
  file_size   BIGINT NOT NULL,
  is_active   BOOLEAN NOT NULL DEFAULT TRUE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
