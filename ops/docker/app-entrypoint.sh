#!/bin/sh
# app-entrypoint.sh — мост «Docker secrets → env» для prod (M11, CLAUDE.md §4.9).
#
# Приложение читает секреты ТОЛЬКО из окружения (config ${ENV_VAR}), а Swarm
# доставляет их файлами в /run/secrets. Секрет app_env — файл KEY=VALUE
# (создаётся `docker secret create app_env -` на сервере, в git не живёт);
# скрипт экспортирует его содержимое и передаёт управление бинарю.
#
# RSA-пара JWT остаётся отдельными файловыми секретами (jwt_private/jwt_public):
# приложение и так берёт их по путям JWT_*_KEY_PATH — экспорт не нужен.
set -eu

if [ -f /run/secrets/app_env ]; then
  set -a
  # shellcheck disable=SC1091
  . /run/secrets/app_env
  set +a
fi

exec "$@"
