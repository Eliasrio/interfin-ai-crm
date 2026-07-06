#!/usr/bin/env bash
# failover_drill.sh — учение Redis failover (M11, критерий IQ-7).
#
# Сценарий: поднять локальный стенд 1 master + 2 replica + 3 Sentinel,
# запустить зонд (очередь + pub/sub через RedisFailoverClientOpt), убить
# master и посмотреть, что:
#   - pub/sub-обрыв обнаруживается мгновенно (< 5 с) — WS Hub уходит в
#     polling fallback, Kanban продолжает работу (IQ-7);
#   - Sentinel промоутит реплику, enqueue оживает (обычно 3–10 с);
#   - сообщения окна недоступности в проде не теряются (§11.2: pending_task
#     + recovery-cron, тест internal/queue/recovery_integration_test.go).
#
# Требования: docker compose, go 1.22 в PATH.
# Запуск из корня репозитория: ./scripts/failover_drill.sh
set -euo pipefail

cd "$(dirname "$0")/.."
COMPOSE="docker compose -f ops/compose/docker-compose.sentinel.yml"
NET="interfin-sentinel-drill_default"
PROBE_DURATION="${PROBE_DURATION:-45s}"

echo "== 1/5: собираю зонд (linux/amd64 — он бежит внутри docker-сети) =="
mkdir -p .drill
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o .drill/failover_probe ./scripts/failover_probe

echo "== 2/5: поднимаю стенд Sentinel =="
$COMPOSE up -d
trap '$COMPOSE down -v >/dev/null 2>&1 || true' EXIT

echo "   жду, пока Sentinel увидит master и обе реплики..."
for i in $(seq 1 30); do
  slaves=$(docker exec drill-sentinel-1 redis-cli -p 26379 sentinel master mymaster 2>/dev/null |
    awk 'prev=="num-slaves"{print; exit} {prev=$0}') || slaves=0
  [ "${slaves:-0}" -ge 2 ] && break
  sleep 1
done
echo "   реплик у master: ${slaves:-0} (нужно 2)"

echo "== 3/5: запускаю зонд на ${PROBE_DURATION} =="
docker rm -f drill-probe >/dev/null 2>&1 || true
# Без --rm: логи зонда нужны ПОСЛЕ его завершения.
docker run -d --name drill-probe --network "$NET" \
  -v "$PWD/.drill/failover_probe:/probe:ro" alpine:3.19 \
  /probe -sentinels sentinel-1:26379,sentinel-2:26380,sentinel-3:26381 \
  -duration "$PROBE_DURATION" >/dev/null

sleep 5
echo "== 4/5: УБИВАЮ master (docker kill drill-redis-m) =="
KILL_TS=$(date +%s)
docker kill drill-redis-m >/dev/null

echo "   жду завершения зонда..."
# Не docker wait: он подвисает на некоторых версиях Docker Desktop.
for i in $(seq 1 120); do
  state=$(docker inspect -f '{{.State.Status}}' drill-probe 2>/dev/null || echo gone)
  [ "$state" = "exited" ] || [ "$state" = "gone" ] && break
  sleep 2
done
echo
echo "===== вывод зонда ====="
docker logs drill-probe
echo "======================="

echo "== 5/5: кто стал новым master =="
docker exec drill-sentinel-1 redis-cli -p 26379 sentinel get-master-addr-by-name mymaster
echo
echo "Учение завершено (master убит в $(date -r "$KILL_TS" '+%H:%M:%S' 2>/dev/null || date -d "@$KILL_TS" '+%H:%M:%S'))."
echo "Критерий IQ-7: строка зонда «ОБРЫВ обнаружен через ...» < 5s."
docker rm -f drill-probe >/dev/null 2>&1 || true
