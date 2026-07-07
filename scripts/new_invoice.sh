#!/bin/bash
# new_invoice.sh — выставить лиду счёт CryptoBot одной командой (прод).
# Печатает платёжную ссылку — её отправляют клиенту; оплата двигает лида
# по доске автоматически (вебхук M6).
#
# Запуск: bash /opt/interfin-ai-crm/scripts/new_invoice.sh <ID лида> <сумма> [валюта]
# Пример: bash /opt/interfin-ai-crm/scripts/new_invoice.sh 1 2000 USDT
set -euo pipefail

if [ $# -lt 2 ]; then
  echo "Использование: bash $0 <ID лида> <сумма> [валюта]"
  echo "Пример:        bash $0 1 2000 USDT"
  exit 1
fi
LEAD="$1"
AMOUNT="$2"
ASSET="${3:-USDT}"

APP="$(docker ps -qf name=crm_app | head -1)"
if [ -z "$APP" ]; then
  echo "✗ Контейнер приложения не найден — пришлите этот вывод в чат Claude."
  exit 1
fi

# docker exec идёт мимо entrypoint — app_env сорсим явно (runbook M11).
docker exec "$APP" sh -c "set -a; . /run/secrets/app_env; set +a; \
  /app/create-invoice -lead '$LEAD' -amount '$AMOUNT' -asset '$ASSET'"
