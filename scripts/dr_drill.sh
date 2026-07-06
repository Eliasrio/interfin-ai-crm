#!/usr/bin/env bash
# dr_drill.sh — DR-учение PITR (M11 §11.3, AQ²-8): восстановление БД на
# точку времени < 5 минут назад из base-бэкапа + WAL-архива.
#
# Локальный контур (MinIO играет роль S3; в prod — S3 sa-east-1, тот же
# wal-g, тот же порядок действий — см. docs/ops/provisioning.md «DR drill»):
#
#   1. MinIO + бакет walg;
#   2. Postgres (образ ops/postgres: pgvector + wal-g), archive_command →
#      wal-g wal-push, archive_timeout 10с (в prod 60с);
#   3. wal-g backup-push (base-бэкап);
#   4. маркер ДО катастрофы → T1 → «плохая» запись ПОСЛЕ;
#   5. катастрофа: контейнер и том уничтожаются;
#   6. wal-g backup-fetch + recovery_target_time=T1 → restore_command тянет
#      WAL из «S3»;
#   7. проверка: маркер «до» есть, записи «после» нет.
#
# Запуск из корня репозитория: ./scripts/dr_drill.sh
set -euo pipefail
cd "$(dirname "$0")/.."

NET=drill-pitr
PG=drill-pitr-pg
MINIO=drill-pitr-minio
VOL=drill-pitr-pgdata
VOL2=drill-pitr-pgdata-restored
IMG=interfin-crm/postgres:16

WALG_ENV=(-e WALG_S3_PREFIX=s3://walg/pitr
  -e AWS_ACCESS_KEY_ID=minioadmin -e AWS_SECRET_ACCESS_KEY=minioadmin
  -e AWS_ENDPOINT=http://drill-pitr-minio:9000 -e AWS_S3_FORCE_PATH_STYLE=true
  -e AWS_REGION=sa-east-1)

cleanup() {
  docker rm -f "$PG" "$MINIO" >/dev/null 2>&1 || true
  docker volume rm "$VOL" "$VOL2" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

echo "== 1/7: собираю образ postgres+wal-g =="
docker build -q -t "$IMG" ops/postgres

echo "== 2/7: поднимаю MinIO (локальный «S3») =="
docker network create "$NET" >/dev/null
docker run -d --name "$MINIO" --network "$NET" \
  -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
  minio/minio:latest server /data >/dev/null
sleep 3
docker run --rm --network "$NET" --entrypoint sh minio/mc:latest -c \
  "mc alias set m http://drill-pitr-minio:9000 minioadmin minioadmin >/dev/null && mc mb m/walg" >/dev/null

echo "== 3/7: поднимаю Postgres с WAL-архивом в «S3» =="
docker run -d --name "$PG" --network "$NET" -v "$VOL":/var/lib/postgresql/data \
  -e POSTGRES_PASSWORD=drill -e POSTGRES_DB=interfin "${WALG_ENV[@]}" \
  "$IMG" postgres \
  -c wal_level=replica -c archive_mode=on \
  -c "archive_command=wal-g wal-push %p" -c archive_timeout=10 >/dev/null
# Ждём ГОТОВЫЙ TCP-listener (-h 127.0.0.1): entrypoint postgres в начале
# поднимает временный сервер только на unix-сокете (initdb-фаза) — сокетный
# pg_isready проходит раньше, чем поднялся боевой сервер с TCP, а wal-g
# ходит именно по TCP.
until docker exec "$PG" pg_isready -h 127.0.0.1 -U postgres -d interfin >/dev/null 2>&1; do sleep 1; done

echo "== 4/7: base-бэкап (wal-g backup-push) =="
docker exec -u postgres -e PGHOST=127.0.0.1 "${WALG_ENV[@]}" "$PG" \
  wal-g backup-push /var/lib/postgresql/data 2>&1 | tail -1

echo "== 5/7: маркер до катастрофы → T1 → «плохая» запись после =="
docker exec "$PG" psql -U postgres -d interfin -qc \
  "CREATE TABLE drill(marker text, ts timestamptz DEFAULT now());
   INSERT INTO drill(marker) VALUES ('before-disaster');"
sleep 2
T1=$(docker exec "$PG" psql -U postgres -d interfin -Atc "SELECT now()")
echo "   точка восстановления T1 = $T1"
sleep 2
docker exec "$PG" psql -U postgres -d interfin -qc \
  "INSERT INTO drill(marker) VALUES ('after-disaster-must-vanish');"
# Дожимаем WAL с обеими записями в архив (в проде это делает archive_timeout).
docker exec "$PG" psql -U postgres -d interfin -qc "SELECT pg_switch_wal();" >/dev/null
sleep 4

echo "== 6/7: КАТАСТРОФА — контейнер и том уничтожены; восстанавливаю на T1 =="
docker rm -f "$PG" >/dev/null
docker volume rm "$VOL" >/dev/null

docker run --rm -v "$VOL2":/var/lib/postgresql/data "$IMG" \
  bash -c 'chown postgres:postgres /var/lib/postgresql/data'
docker run --rm -u postgres --network "$NET" \
  -v "$VOL2":/var/lib/postgresql/data "${WALG_ENV[@]}" "$IMG" \
  wal-g backup-fetch /var/lib/postgresql/data LATEST 2>&1 | tail -1
docker run --rm -u postgres -v "$VOL2":/var/lib/postgresql/data "$IMG" bash -c "
  touch /var/lib/postgresql/data/recovery.signal
  cat >> /var/lib/postgresql/data/postgresql.auto.conf <<CONF
restore_command = 'wal-g wal-fetch %f %p'
recovery_target_time = '$T1'
recovery_target_action = 'promote'
CONF"
docker run -d --name "$PG" --network "$NET" -v "$VOL2":/var/lib/postgresql/data \
  "${WALG_ENV[@]}" "$IMG" postgres >/dev/null
echo "   жду окончания recovery..."
for i in $(seq 1 60); do
  docker exec "$PG" psql -U postgres -d interfin -Atc "SELECT 1" >/dev/null 2>&1 && break
  sleep 2
done

echo "== 7/7: проверка PITR =="
ROWS=$(docker exec "$PG" psql -U postgres -d interfin -Atc "SELECT marker FROM drill ORDER BY ts")
echo "   строки после восстановления: ${ROWS:-<нет>}"
if [ "$ROWS" = "before-disaster" ]; then
  echo "PITR OK: маркер до T1 восстановлен, запись после T1 отсутствует (AQ²-8)."
else
  echo "PITR FAILED: ожидали ровно 'before-disaster'." >&2
  exit 1
fi
