#!/bin/bash
# update_kb.sh — обновление базы знаний бота (docs/kb/*.md) одной командой.
# Делает: git pull → копирование kb в контейнер app → переиндексация Voyage.
# Запуск: bash /opt/interfin-ai-crm/scripts/update_kb.sh
set -euo pipefail
cd "$(dirname "$0")/.."

echo "=== Шаг 1/3: забираю свежий код с GitHub ==="
git pull

if [ -z "$(ls docs/kb/*.md 2>/dev/null)" ]; then
  echo "✗ В docs/kb нет .md-файлов — индексировать нечего."
  exit 1
fi

APP="$(docker ps -qf name=crm_app | head -1)"
if [ -z "$APP" ]; then
  echo "✗ Контейнер приложения не найден — пришлите этот вывод в чат Claude."
  exit 1
fi

echo "=== Шаг 2/3: копирую документы в контейнер ==="
docker cp docs/kb "$APP":/tmp/kb

echo "=== Шаг 3/3: индексирую (docker exec идёт мимо entrypoint — app_env сорсим явно) ==="
docker exec "$APP" sh -c 'set -a; . /run/secrets/app_env; set +a; /app/index-kb -dir /tmp/kb'
docker exec "$APP" rm -rf /tmp/kb

echo
echo "ГОТОВО. Эмма уже видит новую базу знаний (перезапуск не нужен)."
