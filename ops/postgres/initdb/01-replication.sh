#!/bin/bash
# 01-replication.sh — первичная инициализация primary (M11 §11.3).
# Выполняется стандартным entrypoint'ом postgres ТОЛЬКО на пустом PGDATA.
#
# Создаёт роль репликации (пароль — из Docker secret replication_password)
# и разрешает ей вход с backend-сети. Секрет создаётся на сервере:
#   docker secret create replication_password -
set -eu

REPL_PASSWORD="$(cat /run/secrets/replication_password)"

psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" <<SQL
CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD '${REPL_PASSWORD}';
SQL

# Доступ реплике: backend-сеть Swarm (10.0.0.0/8) по scram.
cat >> "$PGDATA/pg_hba.conf" <<HBA
# M11 §11.3: streaming replication из backend-сети
host replication replicator 10.0.0.0/8 scram-sha-256
HBA
