#!/bin/bash
# walg-entrypoint.sh — экспорт wal-g окружения из Docker secret и делегация
# стандартному entrypoint'у postgres (M11 §11.3, CLAUDE.md §4.9).
#
# Секрет walg_env (KEY=VALUE):
#   WALG_S3_PREFIX=s3://interfin-crm-wal/prod
#   AWS_REGION=sa-east-1
#   AWS_ACCESS_KEY_ID=...
#   AWS_SECRET_ACCESS_KEY=...
#   # для локального DR-drill через MinIO дополнительно:
#   # AWS_ENDPOINT=http://minio:9000  и  AWS_S3_FORCE_PATH_STYLE=true
#
# archive_command зовёт wal-g из процесса postgres — окружение обязано быть
# у САМОГО postgres, поэтому экспорт живёт в entrypoint, а не в env compose
# (секреты в env stack-файла запрещены, CLAUDE.md §4.9).
set -eu

if [ -f /run/secrets/walg_env ]; then
  set -a
  # shellcheck disable=SC1091
  . /run/secrets/walg_env
  set +a
fi

exec docker-entrypoint.sh "$@"
