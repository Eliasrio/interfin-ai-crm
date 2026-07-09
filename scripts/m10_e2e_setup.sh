#!/usr/bin/env bash
# m10_e2e_setup.sh — данные для e2e-тестов фронта M10 (web/e2e/).
# Нужен поднятый docker compose (postgres + app). Идемпотентен: тестовые
# учётки пересоздаются, тестовый лид возвращается на стадию 2.
#
# Использование:
#   ./scripts/m10_e2e_setup.sh
#   cd web && E2E_BASE=http://localhost:8080 npm run test:e2e
set -euo pipefail
cd "$(dirname "$0")/.."

PSQL=(docker compose exec -T postgres psql -U postgres -d interfin -qtA)

echo "== тестовые менеджеры e2e-m10-* (пароль: e2e-m10-pass)"
"${PSQL[@]}" -c "DELETE FROM refresh_tokens WHERE manager_id IN (SELECT id FROM managers WHERE email LIKE 'e2e-m10-%');"
"${PSQL[@]}" -c "DELETE FROM managers WHERE email LIKE 'e2e-m10-%';"

export PATH="$HOME/sdk/go1.22.12/bin:$PATH" GOTOOLCHAIN=go1.22.12
export POSTGRES_DSN='postgres://postgres:postgres@localhost:5432/interfin?sslmode=disable'
printf 'e2e-m10-pass\n' | go run ./cmd/create-manager -email e2e-m10-ui@interfin.com -name 'E2E UI' -role manager
printf 'e2e-m10-pass\n' | go run ./cmd/create-manager -email e2e-m10-actor@interfin.com -name 'E2E Actor' -role manager
# M13: секция «Настройки» видна только admin (takeover.e2e.test.jsx).
printf 'e2e-m10-pass\n' | go run ./cmd/create-manager -email e2e-m10-admin@interfin.com -name 'E2E Admin' -role admin

echo "== тестовый лид 'M10 E2E Лид' на стадии 2"
# telegram_user_id уникален НА КАЖДЫЙ прогон: LGPD-хеш при erasure
# детерминированный (§9.3), повторный erase того же tg id упирается в
# UNIQUE leads_telegram_user_id_key. Тест ищет лида по имени, не по id.
"${PSQL[@]}" -c "DELETE FROM leads WHERE name = 'M10 E2E Лид';" # messages каскадом (0004)
"${PSQL[@]}" -c "INSERT INTO leads (telegram_user_id, name, tg_username, stage_id, message_count, last_activity_at)
  VALUES (9990000000 + (extract(epoch from now())::bigint % 999999937), 'M10 E2E Лид', 'm10_e2e', 2, 7, NOW());"

echo "== готово"
