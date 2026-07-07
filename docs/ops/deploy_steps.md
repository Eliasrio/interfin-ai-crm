# Деплой на боевой сервер — пошаговая инструкция исполнителя

Последовательность команд «сверху вниз», без ветвлений. Обоснования и
альтернативы (RDS vs self-hosted и т.п.) — в [provisioning.md](provisioning.md).
Здесь — self-hosted вариант PostgreSQL (все конфиги в репозитории).

Время выполнения: 2–4 часа. Помечено: 🔑 — нужен ключ/решение владельца,
✅ — контрольная точка, дальше не идти, пока не сойдётся.

---

## Фаза 0. Собрать заранее (до сервера)

🔑 Владелец продукта готовит:

| Что | Где взять |
|---|---|
| Боевой Telegram-бот | @BotFather → `/newbot` (НЕ дев-бот) → токен |
| Chat ID менеджеров | добавить бота в чат менеджеров, узнать id (@userinfobot) |
| Ключ Anthropic (prod) | console.anthropic.com → API Keys → отдельный ключ |
| Ключ Voyage (prod) | dash.voyageai.com → платный тариф (free 3 RPM — мало) |
| CryptoBot mainnet | @CryptoBot → Crypto Pay → Create App → токен |
| Домен | A-запись `crm.<домен>` → Elastic IP сервера (после фазы 1) |

## Фаза 1. Сервер AWS (15 мин)

1. EC2, регион **sa-east-1 (São Paulo)**: Ubuntu 24.04 LTS, `t3.xlarge`
   (4 vCPU / 16 ГБ), диск gp3 100 ГБ.
2. Elastic IP → привязать к инстансу → прописать DNS A-запись.
3. Security Group: **22/tcp только с IP офиса/VPN**, 80/tcp и 443/tcp — отовсюду.
4. S3-бакет `interfin-crm-wal` в sa-east-1 (приватный, versioning off).
5. Два IAM-пользователя с programmatic access:
   - `walg` — политика только на бакет `interfin-crm-wal` (Get/Put/List/Delete);
   - `crm-app` — политика на бакет файлов приложения `interfin-crm-files`.

✅ `ping crm.<домен>` отвечает с Elastic IP.

## Фаза 2. Базовая настройка сервера (10 мин)

```bash
ssh ubuntu@crm.<домен>
sudo -i

apt update && apt upgrade -y
curl -fsSL https://get.docker.com | sh
docker swarm init

ufw default deny incoming
ufw allow 22/tcp && ufw allow 80/tcp && ufw allow 443/tcp
ufw enable
```

✅ `docker node ls` показывает один узел-менеджер; `ufw status` — три правила.

## Фаза 3. TLS и mTLS (15 мин)

```bash
# Let's Encrypt (порт 80 пока свободен):
apt install -y certbot
certbot certonly --standalone -d crm.<домен>
# автопродление + перезапуск nginx стека:
echo 'renew_hook = docker service update --force crm_nginx' \
  >> /etc/letsencrypt/renewal/crm.<домен>.conf

# mTLS-CA для /metrics (AQ²-10):
mkdir -p /etc/interfin/metrics-ca && cd /etc/interfin/metrics-ca
openssl req -x509 -newkey rsa:4096 -nodes -days 1825 \
  -keyout ca.key -out ca.crt -subj "/CN=interfin-metrics-ca"
# клиентский сертификат мониторинг-хоста/админа:
openssl req -newkey rsa:2048 -nodes -keyout client.key -out client.csr -subj "/CN=ops"
openssl x509 -req -in client.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -days 365 -out client.crt
chmod 600 ca.key client.key
# client.crt + client.key передать админу (ими ходят на /metrics)
```

✅ `ls /etc/letsencrypt/live/crm.<домен>/fullchain.pem` существует.

## Фаза 4. Код и правки под домен (10 мин)

```bash
cd /opt && git clone https://github.com/Eliasrio/interfin-ai-crm && cd interfin-ai-crm

# подставить боевой домен вместо примера:
sed -i 's/crm\.interfin\.example/crm.<домен>/g' ops/nginx/conf.d/crm.conf

# allowlist /metrics: заменить блок allow в location /metrics
# на реальные IP офиса/VPN (nano ops/nginx/conf.d/crm.conf)
```

✅ `grep -c 'crm.<домен>' ops/nginx/conf.d/crm.conf` ≥ 4; примера в файле нет.

## Фаза 5. Секреты (30 мин, самая ответственная)

Пароли генерировать и СРАЗУ класть в менеджер паролей команды — некоторые
понадобятся в следующих фазах.

```bash
# 1) сгенерировать и записать себе: PG_SUPER, PG_CRM, PG_REPL, PG_BOUNCER_ADMIN, REDIS_PASS
openssl rand -base64 24   # повторить 5 раз, записать под этими именами

echo -n "<PG_SUPER>"  | docker secret create postgres_password -
echo -n "<PG_REPL>"   | docker secret create replication_password -
echo -n "<REDIS_PASS>"| docker secret create redis_password -
openssl rand -base64 24 | tee /dev/tty | docker secret create grafana_admin_password -

# 2) JWT-пара — СВЕЖАЯ, дев-ключи не переносить:
./scripts/gen_jwt_keys.sh
docker secret create jwt_private secrets/jwt_private.pem
docker secret create jwt_public  secrets/jwt_public.pem
rm -rf secrets/

# 3) app_env — все секретные env приложения одним файлом:
cat > app.env <<'ENV'
POSTGRES_DSN=postgres://crm:<PG_CRM>@pgbouncer:6432/interfin?sslmode=disable
REDIS_PASSWORD=<REDIS_PASS>
TELEGRAM_BOT_TOKEN=<токен боевого бота>
TELEGRAM_WEBHOOK_SECRET=<openssl rand -hex 32>
TELEGRAM_WEBHOOK_URL=https://crm.<домен>/webhook/telegram
TELEGRAM_MANAGER_CHAT_ID=<id чата менеджеров>
ANTHROPIC_API_KEY=<prod-ключ>
VOYAGE_API_KEY=<prod-ключ>
CRYPTOBOT_TESTNET_TOKEN=<тестнет-токен, обязателен формально>
CRYPTOBOT_MAINNET_TOKEN=<mainnet-токен>
LGPD_SALT=<openssl rand -hex 32 — ПОСЛЕ ПЕРВОГО ERASURE НЕ МЕНЯТЬ НИКОГДА>
AWS_ACCESS_KEY_ID=<IAM crm-app>
AWS_SECRET_ACCESS_KEY=<IAM crm-app>
ENV
docker secret create app_env app.env && shred -u app.env

# 4) wal-g → S3:
cat > walg.env <<'ENV'
WALG_S3_PREFIX=s3://interfin-crm-wal/prod
AWS_REGION=sa-east-1
AWS_ACCESS_KEY_ID=<IAM walg>
AWS_SECRET_ACCESS_KEY=<IAM walg>
ENV
docker secret create walg_env walg.env && shred -u walg.env

# 5) Alertmanager (токен бота + chat id внутри):
cp ops/alertmanager/alertmanager.yml.example alertmanager.yml
nano alertmanager.yml    # bot_token, chat_id
docker secret create alertmanager_yml alertmanager.yml && shred -u alertmanager.yml
```

Секрет №10 (`pgbouncer_userlist`) создаётся в фазе 6 — ему нужны
SCRAM-верификаторы из работающего Postgres.

✅ `docker secret ls` — 9 секретов.

## Фаза 6. Инициализация БД и pgbouncer_userlist (20 мин)

pgbouncer авторизует по SCRAM-верификаторам из `pg_authid`, поэтому БД
инициализируется ДО деплоя стека — временным контейнером на том же томе,
который потом подхватит стек (`crm_pgdata-primary`).

```bash
docker compose -f docker-compose.prod.yml build   # собирает и app, и postgres+wal-g

docker volume create crm_pgdata-primary
echo -n "<PG_REPL>" > /tmp/repl_pass
docker run -d --name pg-init \
  -v crm_pgdata-primary:/var/lib/postgresql/data \
  -v /tmp/repl_pass:/run/secrets/replication_password:ro \
  -v $PWD/ops/postgres/initdb:/docker-entrypoint-initdb.d:ro \
  -e POSTGRES_PASSWORD='<PG_SUPER>' -e POSTGRES_DB=interfin \
  interfin-crm/postgres:16
until docker exec pg-init pg_isready -h 127.0.0.1 -U postgres; do sleep 2; done

docker exec -it pg-init psql -U postgres -d interfin <<'SQL'
CREATE ROLE crm LOGIN PASSWORD '<PG_CRM>';
ALTER DATABASE interfin OWNER TO crm;
CREATE ROLE pgbouncer_admin LOGIN PASSWORD '<PG_BOUNCER_ADMIN>';
SELECT rolname, rolpassword FROM pg_authid
 WHERE rolname IN ('crm','pgbouncer_admin');
SQL

# из вывода SELECT собрать userlist.txt (формат — две строки):
cat > userlist.txt <<'EOF'
"crm" "SCRAM-SHA-256$4096:...из вывода..."
"pgbouncer_admin" "SCRAM-SHA-256$4096:...из вывода..."
EOF
docker secret create pgbouncer_userlist userlist.txt && shred -u userlist.txt

docker stop pg-init && docker rm pg-init && rm /tmp/repl_pass
```

✅ `docker secret ls` — 10 секретов; том `crm_pgdata-primary` существует.

## Фаза 7. Деплой стека и миграции (15 мин)

```bash
docker stack deploy -c docker-compose.prod.yml crm
watch docker service ls        # ждать: все 1/1 и 2/2 (первый раз 2–3 мин)

# миграции — суперпользователем, напрямую в postgres (мимо pgbouncer),
# один раз (CREATE EXTENSION vector требует суперпользователя):
docker run --rm --network crm_backend \
  -e POSTGRES_DSN='postgres://postgres:<PG_SUPER>@postgres-primary:5432/interfin?sslmode=disable' \
  interfin-crm/app:latest /app/migrate up

# первый base-бэкап wal-g (дальше по расписанию/вручную):
docker exec -u postgres $(docker ps -qf name=crm_postgres-primary) \
  bash -c 'set -a; . /run/secrets/walg_env; set +a; wal-g backup-push $PGDATA'
```

✅ `docker service ls`: published-порты только 80/443 у nginx.
✅ `curl https://crm.<домен>/health` → `{"status":"ok"}`.
✅ `curl 'https://api.telegram.org/bot<токен>/getWebhookInfo'` → `url` боевой,
   `last_error_message` пуст (вебхук приложение регистрирует само при старте).
✅ `wal-g backup-list` (той же командой exec) показывает один бэкап.

## Фаза 8. Первичные данные (15 мин)

```bash
# учётка администратора доски (пароль спросит интерактивно):
docker run --rm -it --network crm_backend \
  -e POSTGRES_DSN='postgres://crm:<PG_CRM>@pgbouncer:6432/interfin?sslmode=disable' \
  interfin-crm/app:latest /app/create-manager -email admin@<домен> -role admin

# база знаний RAG: наполнить docs/kb боевыми .md-документами, затем:
docker run --rm --network crm_backend \
  -v $PWD/docs/kb:/kb:ro \
  -e POSTGRES_DSN='postgres://crm:<PG_CRM>@pgbouncer:6432/interfin?sslmode=disable' \
  -e VOYAGE_API_KEY='<prod-ключ>' \
  interfin-crm/app:latest /app/index-kb -dir /kb
```

После наполнения реальной базы знаний откалибровать порог RAG
(`rag.cosine_threshold`, сейчас временный 0.40) — `scripts/rag_calibrate`.

✅ Вход на `https://crm.<домен>/` учёткой admin работает, доска открывается.

## Фаза 9. Чек-лист безопасности (обязательный, с ВНЕШНЕЙ машины)

```bash
# порты закрыты:
nc -zv -w3 crm.<домен> 5432 6379 26379 9090 9091 3000   # все: timed out/refused
# metrics: двойной замок
curl -s -o /dev/null -w '%{http_code}\n' https://crm.<домен>/metrics          # 403
curl -s -o /dev/null -w '%{http_code}\n' \
  --cert client.crt --key client.key https://crm.<домен>/metrics              # 200 (с IP из allowlist)
```

На сервере:
- [ ] `docker secret ls` — 10; `git -C /opt/interfin-ai-crm status` чист, `.env` не создавался;
- [ ] `ufw status` — только 22/80/443;
- [ ] Grafana открывается только через `https://crm.<домен>/grafana/` (пароль из секрета);
- [ ] SMOKE платежей: тестовый инвойс в mainnet на минимальную сумму, оплата, лид перешёл по стадии.

## Фаза 10. Учения и алертинг (30 мин)

```bash
# алерт-смоук: погасить один sentinel → в чат менеджеров придёт RedisSentinelDown
docker service scale crm_sentinel-3=0 && sleep 90
docker service scale crm_sentinel-3=1

# DR-drill на проде (раз в квартал, инструкция §8 provisioning.md):
# wal-g backup-fetch на чистый том + recovery_target_time < 5 мин назад
```

✅ Алерт пришёл в Telegram; после восстановления — resolved.

## Эксплуатация

- Обновление версии: `git pull && docker compose -f docker-compose.prod.yml build && docker stack deploy -c docker-compose.prod.yml crm` (rolling, start-first).
- Дашборды: `https://crm.<домен>/grafana/` → папка Interfin CRM.
- Логи: `docker service logs crm_app --since 1h`.
- Раз в квартал: DR-drill + ротация клиентских mTLS-сертификатов.
