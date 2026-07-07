#!/bin/bash
# replica-entrypoint.sh — запуск hot-standby реплики (M11 §11.3).
#
# Пустой PGDATA → pg_basebackup с primary (слот создаётся автоматически),
# файл standby.signal переводит postgres в режим реплики; дальше обычный
# запуск. Непустой PGDATA → сразу запуск (рестарты не перекачивают базу).
set -eu

if [ -f /run/secrets/walg_env ]; then
  set -a
  # shellcheck disable=SC1091
  . /run/secrets/walg_env
  set +a
fi

PRIMARY_HOST="${PRIMARY_HOST:-postgres-primary}"
export PGPASSWORD="$(cat /run/secrets/replication_password)"

if [ ! -s "$PGDATA/PG_VERSION" ]; then
  echo "replica: пустой PGDATA — снимаю базовую копию с ${PRIMARY_HOST}"
  until pg_isready -h "$PRIMARY_HOST" -U replicator; do
    echo "replica: жду primary..."
    sleep 2
  done
  pg_basebackup \
    --host="$PRIMARY_HOST" \
    --username=replicator \
    --pgdata="$PGDATA" \
    --wal-method=stream \
    --create-slot --slot=replica_1 \
    --write-recovery-conf \
    --progress
  chmod 0700 "$PGDATA"
fi

unset PGPASSWORD
exec docker-entrypoint.sh postgres -c hot_standby=on
