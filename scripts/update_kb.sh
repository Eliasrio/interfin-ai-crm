#!/bin/bash
# update_kb.sh — обновление базы знаний бота (docs/kb/*.md) одной командой.
# Делает: git pull → копирование kb в контейнер app → переиндексация Voyage.
# README.md служебный (инструкции контент-команде) — в индекс не попадает.
# Запуск: bash /opt/interfin-ai-crm/scripts/update_kb.sh
set -euo pipefail
cd "$(dirname "$0")/.."

echo "=== Шаг 1/3: забираю свежий код с GitHub ==="
git pull

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
cp docs/kb/*.md "$STAGE"/ 2>/dev/null || true
cp docs/kb/*.txt "$STAGE"/ 2>/dev/null || true
rm -f "$STAGE/README.md" "$STAGE/testset.json"
if [ -z "$(ls "$STAGE" 2>/dev/null)" ]; then
  echo "✗ В docs/kb нет документов (кроме служебного README) — индексировать нечего."
  exit 1
fi

APP="$(docker ps -qf name=crm_app | head -1)"
if [ -z "$APP" ]; then
  echo "✗ Контейнер приложения не найден — пришлите этот вывод в чат Claude."
  exit 1
fi

echo "=== Шаг 2/3: копирую документы в контейнер ==="
# docker cp пишет файлы от root, приложение работает не от root —
# чистим тоже от root (-u root), иначе rm упирается в permission denied.
docker exec -u root "$APP" rm -rf /tmp/kb
docker cp "$STAGE" "$APP":/tmp/kb

echo "=== Шаг 3/3: индексирую (docker exec идёт мимо entrypoint — app_env сорсим явно) ==="
docker exec "$APP" sh -c 'set -a; . /run/secrets/app_env; set +a; /app/index-kb -dir /tmp/kb'
docker exec -u root "$APP" rm -rf /tmp/kb

echo
echo "ГОТОВО. Эмма уже видит новую базу знаний (перезапуск не нужен)."
