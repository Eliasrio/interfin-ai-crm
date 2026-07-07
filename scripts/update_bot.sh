#!/bin/bash
# update_bot.sh — обновление бота на боевом сервере одной командой.
# Делает: git pull → сборка образа app → rolling-перезапуск → проверка.
# Запуск: bash /opt/interfin-ai-crm/scripts/update_bot.sh
set -euo pipefail
cd "$(dirname "$0")/.."

echo "=== Шаг 1/4: забираю свежий код с GitHub ==="
git pull
echo
echo "Код на сервере: $(git log --oneline -1)"
echo

echo "=== Шаг 2/4: собираю приложение (2-5 минут, ждите) ==="
docker compose -f docker-compose.prod.yml build app

echo
echo "=== Шаг 3/4: перезапускаю приложение (без простоя, по одному) ==="
docker service update --force crm_app

echo
echo "=== Шаг 4/4: проверяю ==="
sleep 5
if curl -fsS https://crm.borninbrazil.baby/health >/dev/null; then
  echo "✓ сайт отвечает"
else
  echo "✗ сайт НЕ отвечает — пришлите этот вывод в чат Claude"
  exit 1
fi

APP="$(docker ps -qf name=crm_app | head -1)"
if [ -n "$APP" ]; then
  echo "✓ версия в работе: $(git log --oneline -1)"
fi

echo
echo "ГОТОВО. Подождите ~30 секунд и проверьте бота в Telegram."
