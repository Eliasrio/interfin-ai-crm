# M7 — Аутентификация и авторизация

**Ветка:** `feat/m7-auth`
**Зависимости:** M0, M1
**Можно параллельно с:** M2→M3→M4
**Разделы SRS:** §5 (5.1 JWT, 5.2 роли, 5.3 WS auth)

## Цель
JWT RS256 + Refresh Token, middleware авторизации, роли. Основа для защиты
REST API (M8) и WebSocket (M9).

## Задачи
1. **JWT RS256** (§5.1):
   - Access Token: JWT RS256, TTL 15 мин (`access_token_ttl: 900`);
   - Refresh Token: opaque UUID, TTL 7 дней, хранится в HttpOnly cookie;
   - payload `{ sub, role: admin|manager, iat, exp }`;
   - ключи из Docker secrets (`jwt_private.pem`, `jwt_public.pem`).
2. `POST /auth/login` → `{ access_token, expires_in: 900 }` + set refresh cookie.
3. `POST /auth/refresh`: авторизуется **по Refresh Token из cookie**, НЕ по
   Access JWT (важно — иначе refresh бессмыслен при истёкшем access). Возвращает
   новый access token.
4. Auth middleware: валидация Bearer JWT; истёкший → 401; неверная роль → 403.
5. **Роли** (§5.2):
   - `manager` — свои лиды (`/api/leads/*`, `/api/lgpd/*`);
   - `admin` — все лиды + LGPD;
   - `system` — internal endpoints.
6. **WS auth helper** (§5.3): валидация JWT из `Sec-WebSocket-Protocol:
   Bearer.<token>` при upgrade (использует M9).
7. Хранилище менеджеров (таблица managers — добавить миграцию) + хеш паролей
   (bcrypt/argon2).

## Контракт наружу
- Auth middleware для Gin — оборачивает защищённые роуты в M8.
- WS auth helper — для upgrade в M9.
- Роли-гейт для эндпоинтов.

## Критерии приёмки
- [ ] Просроченный JWT → 401; неверная роль → 403 (AQ²-2 auth test).
- [ ] `/auth/refresh` работает при истёкшем access token (по refresh cookie).
- [ ] Пароли не хранятся в открытом виде.
- [ ] JWT-ключи только из Docker secrets, не в коде.
