-- 0009 (M7): менеджеры и refresh-токены (SRS §5.1–5.2).
--
-- managers: учётки сотрудников CRM. password_hash — ТОЛЬКО bcrypt
-- (критерий приёмки M7: пароли не хранятся в открытом виде).
-- role повторяет payload JWT { role: admin|manager } (§5.1); роль system
-- в таблице не живёт — это internal-токены сервисов, а не люди (§5.2).
CREATE TABLE managers (
  id            BIGSERIAL PRIMARY KEY,
  email         VARCHAR(255) NOT NULL UNIQUE,
  name          VARCHAR(255),
  password_hash VARCHAR(255) NOT NULL,
  role          VARCHAR(16)  NOT NULL CHECK (role IN ('admin', 'manager')),
  active        BOOLEAN      NOT NULL DEFAULT TRUE,
  created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- refresh_tokens (§5.1): opaque UUID живёт только в HttpOnly cookie клиента;
-- в БД — hex(SHA-256) токена: утечка таблицы не даёт готовых сессий.
-- Ротация: /auth/refresh атомарно удаляет строку (DELETE ... RETURNING,
-- repo.Consume) и выдаёт новый токен — повторное предъявление старого = 401.
CREATE TABLE refresh_tokens (
  id          BIGSERIAL PRIMARY KEY,
  token_hash  CHAR(64)    NOT NULL UNIQUE,
  manager_id  BIGINT      NOT NULL REFERENCES managers(id) ON DELETE CASCADE,
  expires_at  TIMESTAMPTZ NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_refresh_tokens_manager ON refresh_tokens(manager_id);
-- Опорный индекс для чистки просроченных строк (repo.DeleteExpired).
CREATE INDEX idx_refresh_tokens_expires ON refresh_tokens(expires_at);
