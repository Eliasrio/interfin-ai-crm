#!/usr/bin/env bash
# metrics_mtls_test.sh — security-тест AQ²-10 (M11 §14): «/metrics с чужого
# IP → 403, с mTLS → 200» на РЕАЛЬНОМ nginx с теми же правилами location
# /metrics, что в ops/nginx/conf.d/crm.conf.
#
# Локальный контур: одноразовый CA + серверный + клиентский сертификаты,
# nginx на dev-сети docker (видит app:9091), curl-клиент из соседнего
# контейнера (IP 172.16/12). Три проверки:
#   A. без клиентского сертификата            → 403 (mTLS-слой);
#   B. с сертификатом, IP в allowlist         → 200;
#   C. с сертификатом, IP ВНЕ allowlist       → 403 (IP-слой, «чужой IP»).
#
# Требования: docker, openssl, запущенный dev-стенд (docker compose up -d).
# Запуск из корня репозитория: ./scripts/metrics_mtls_test.sh
set -euo pipefail
cd "$(dirname "$0")/.."

NET=interfin-ai-crm_default
NGINX=mtls-test-nginx
DIR=$(mktemp -d)
trap 'docker rm -f "$NGINX" >/dev/null 2>&1 || true; rm -rf "$DIR"' EXIT

echo "== 1/4: одноразовый CA + сертификаты =="
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=drill-metrics-ca" \
  -keyout "$DIR/ca.key" -out "$DIR/ca.crt" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj "/CN=mtls-test-nginx" \
  -keyout "$DIR/server.key" -out "$DIR/server.csr" 2>/dev/null
openssl x509 -req -in "$DIR/server.csr" -CA "$DIR/ca.crt" -CAkey "$DIR/ca.key" \
  -CAcreateserial -days 1 -out "$DIR/server.crt" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj "/CN=ops-client" \
  -keyout "$DIR/client.key" -out "$DIR/client.csr" 2>/dev/null
openssl x509 -req -in "$DIR/client.csr" -CA "$DIR/ca.crt" -CAkey "$DIR/ca.key" \
  -CAcreateserial -days 1 -out "$DIR/client.crt" 2>/dev/null

# Конфиг — те же правила /metrics, что в ops/nginx/conf.d/crm.conf
# (mTLS optional на server + обязательность и allowlist в location).
nginx_conf() { # $1 = allow-подсеть «своих»
  cat > "$DIR/nginx.conf" <<CONF
events {}
http {
  server {
    listen 8443 ssl;
    ssl_certificate     /certs/server.crt;
    ssl_certificate_key /certs/server.key;
    ssl_client_certificate /certs/ca.crt;
    ssl_verify_client optional;
    location /metrics {
      if (\$ssl_client_verify != SUCCESS) {
        return 403 '{"error":"client certificate required","code":"ERR_METRICS_FORBIDDEN"}';
      }
      allow $1;
      deny all;
      proxy_pass http://app:9091/metrics;
    }
  }
}
CONF
}

echo "== 2/4: nginx с allowlist 172.16.0.0/12 (docker-сеть = «свои») =="
nginx_conf "172.16.0.0/12"
docker run -d --name "$NGINX" --network "$NET" \
  -v "$DIR/nginx.conf":/etc/nginx/nginx.conf:ro -v "$DIR":/certs:ro \
  nginx:1.27-alpine >/dev/null
sleep 2

curl_code() { # $1 = доп. аргументы curl
  # shellcheck disable=SC2086
  docker run --rm --network "$NET" -v "$DIR":/certs:ro curlimages/curl:8.8.0 \
    -sk -o /dev/null -w '%{http_code}' $1 "https://$NGINX:8443/metrics"
}

echo "== 3/4: проверки mTLS-слоя =="
NO_CERT=$(curl_code "")
WITH_CERT=$(curl_code "--cert /certs/client.crt --key /certs/client.key")
echo "   без клиентского сертификата : $NO_CERT (ждём 403)"
echo "   с сертификатом, IP разрешён : $WITH_CERT (ждём 200)"

echo "== 4/4: IP-слой — allowlist сужен до 10.99.99.0/24, клиент = «чужой IP» =="
nginx_conf "10.99.99.0/24"
docker exec "$NGINX" nginx -s reload >/dev/null 2>&1
sleep 1
FOREIGN=$(curl_code "--cert /certs/client.crt --key /certs/client.key")
echo "   с сертификатом, чужой IP    : $FOREIGN (ждём 403)"

if [ "$NO_CERT" = 403 ] && [ "$WITH_CERT" = 200 ] && [ "$FOREIGN" = 403 ]; then
  echo "AQ²-10 OK: без mTLS → 403, с mTLS из allowlist → 200, чужой IP → 403."
else
  echo "AQ²-10 FAILED" >&2
  exit 1
fi
