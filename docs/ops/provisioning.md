# Провижининг боевого сервера (M11 §13, задача 6)

Пошаговый план вывода Interfin AI-CRM в прод. Все конфиги уже в репозитории
(`docker-compose.prod.yml`, `ops/`); человек на сервере выполняет шаги ниже и
проходит чек-лист безопасности. Первый деплой разумно доверить
DevOps-специалисту разово; дальнейшая эксплуатация — по этому документу.

---

## 1. Где хостить

**Рекомендация по умолчанию: AWS, регион `sa-east-1` (São Paulo).**
Данные бразильских клиентов остаются в Бразилии (LGPD), и это тот же регион,
что для S3-бакета WAL-архива — восстановление не тянет данные через океан.

| Вариант | Когда выбирать |
|---|---|
| **EC2 sa-east-1** (рекомендация) | LGPD-консервативный вариант; RDS рядом |
| Hetzner / DigitalOcean (ЕС) | Если бюджет важнее близости данных; LGPD допускает трансфер при должных мерах (ст. 33), но это надо оформить в политике |

**Конфигурация старта:** ~4 vCPU / 8–16 ГБ RAM, 80+ ГБ SSD
(EC2 `t3.xlarge`/`m7i-flex.large` или VPS аналогичного класса).
Весь stack (`docker-compose.prod.yml`) рассчитан на один такой узел;
Swarm позволяет позже добавить второй узел без переписывания конфигов.

**PostgreSQL: управляемый или свой?**

- **AWS RDS for PostgreSQL (рекомендация):** репликация, бэкапы, PITR и
  failover — забота AWS. Из stack-файла тогда убираются `postgres-primary`,
  `postgres-replica`, а `pgbouncer` смотрит на endpoint RDS. Требования:
  движок PostgreSQL 16 + расширение `vector` (доступно в RDS), Multi-AZ,
  automated backups ≥ 7 дней, PITR включён.
- **Self-hosted по §11.3 (конфиги готовы):** `ops/postgres/` — primary,
  streaming replica, wal-g → S3. Дешевле, но DR-учения (см. §8 ниже) — ваша
  регулярная обязанность.

---

## 2. Матрица окружений

| | dev | staging | prod |
|---|---|---|---|
| Файл | `docker-compose.yml` | `docker-compose.prod.yml` (1 replica app) | `docker-compose.prod.yml` |
| Запуск | `docker compose up` | `docker stack deploy` | `docker stack deploy` |
| Redis | одиночный | Sentinel 1+2+3 | Sentinel 1+2+3 (§11.1) |
| Postgres | один контейнер | primary+replica или RDS-стейджинг | primary+replica+WAL→S3 или RDS |
| pgbouncer | есть (порт 6432, для AQ²-3) | transaction, pool 36 | transaction, pool 36 (§11.3) |
| Секреты | `.env` (не в git) | Docker secrets | Docker secrets (§4.9) |
| TLS | нет (ngrok для вебхука) | Let's Encrypt на staging-домене | Let's Encrypt, боевой домен |
| CryptoBot | testnet | testnet | **mainnet** (`CRYPTOBOT_USE_TESTNET=false`) |
| Telegram webhook | ngrok | staging-домен | боевой домен (не ngrok!) |
| /metrics | localhost | nginx mTLS + allowlist | nginx mTLS + allowlist (AQ²-10) |

---

## 3. Подготовка сервера

```bash
# Ubuntu 24.04 LTS
curl -fsSL https://get.docker.com | sh
docker swarm init                         # single-node Swarm

# firewall: наружу ТОЛЬКО 22 (SSH, лучше с allowlist офиса/VPN), 80, 443
ufw default deny incoming
ufw allow 22/tcp
ufw allow 80/tcp
ufw allow 443/tcp
ufw enable
```

БД/Redis/Sentinel/Prometheus/Grafana/metrics НЕ публикуют портов на хост
(проверьте `docker stack deploy` → `docker service ls`: published только
80/443 у nginx) — снаружи их не существует.

## 4. Секреты (все — до первого deploy)

```bash
# один KEY=VALUE файл со всеми секретными env приложения:
#   POSTGRES_DSN=postgres://crm:<пароль>@pgbouncer:6432/interfin?sslmode=disable
#   REDIS_PASSWORD=...
#   TELEGRAM_BOT_TOKEN=... TELEGRAM_WEBHOOK_SECRET=... TELEGRAM_WEBHOOK_URL=https://<домен>/webhook/telegram
#   TELEGRAM_MANAGER_CHAT_ID=...
#   ANTHROPIC_API_KEY=... VOYAGE_API_KEY=...
#   CRYPTOBOT_TESTNET_TOKEN=... CRYPTOBOT_MAINNET_TOKEN=...
#   LGPD_SALT=...            # НЕ менять после первых erasure (§9.3)!
#   AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...   # S3-загрузки приложения
vim app.env && docker secret create app_env app.env && shred -u app.env

./scripts/gen_jwt_keys.sh                 # локально, затем:
docker secret create jwt_private secrets/jwt_private.pem
docker secret create jwt_public  secrets/jwt_public.pem

openssl rand -base64 32 | docker secret create postgres_password -
openssl rand -base64 32 | docker secret create replication_password -
openssl rand -base64 32 | docker secret create redis_password -
openssl rand -base64 32 | docker secret create grafana_admin_password -

# wal-g → S3 (self-hosted Postgres; для RDS не нужен):
#   WALG_S3_PREFIX=s3://interfin-crm-wal/prod
#   AWS_REGION=sa-east-1
#   AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...   # IAM-роль только на этот бакет
vim walg.env && docker secret create walg_env walg.env && shred -u walg.env

# pgbouncer auth (SCRAM-верификаторы из pg_authid, см. ops/pgbouncer/userlist.txt.example)
docker secret create pgbouncer_userlist userlist.txt && shred -u userlist.txt

# Alertmanager (содержит токен бота — потому секрет целиком):
cp ops/alertmanager/alertmanager.yml.example alertmanager.yml   # заполнить
docker secret create alertmanager_yml alertmanager.yml && shred -u alertmanager.yml
```

## 5. TLS и mTLS

```bash
# Let's Encrypt (стандартный certbot на хосте; nginx монтирует /etc/letsencrypt):
apt install -y certbot
certbot certonly --standalone -d crm.<домен>   # до первого deploy (порт 80 свободен)
# далее автопродление: certbot renew --deploy-hook 'docker service update --force crm_nginx'

# CA для mTLS /metrics (AQ²-10): свой мини-CA, клиентские сертификаты — только
# мониторинг-хостам:
mkdir -p /etc/interfin/metrics-ca && cd /etc/interfin/metrics-ca
openssl req -x509 -newkey rsa:4096 -nodes -days 1825 -keyout ca.key -out ca.crt -subj "/CN=interfin-metrics-ca"
# клиентский сертификат (например, для laptop админа):
openssl req -newkey rsa:2048 -nodes -keyout client.key -out client.csr -subj "/CN=ops"
openssl x509 -req -in client.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 365 -out client.crt
```

В `ops/nginx/conf.d/crm.conf` заменить `crm.interfin.example` на боевой домен
и подставить реальные адреса в `allow`-список `/metrics`.

## 6. Деплой

```bash
git clone https://github.com/Eliasrio/interfin-ai-crm && cd interfin-ai-crm
docker compose -f docker-compose.prod.yml build
docker stack deploy -c docker-compose.prod.yml crm
docker service ls          # все реплики 1/1 и 2/2, published — только 80/443

# миграции (однократно на новой БД):
docker exec $(docker ps -qf name=crm_app | head -1) /app/migrate up
```

Telegram-вебхук регистрируется приложением при старте на
`TELEGRAM_WEBHOOK_URL` из `app_env` — боевой домен с валидным TLS
(Telegram не принимает self-signed без явной загрузки сертификата).
Проверка: `https://api.telegram.org/bot<token>/getWebhookInfo` → `url`
боевой, `last_error_message` пуст.

## 7. Чек-лист безопасности перед запуском (обязательный)

- [ ] `ufw status`: наружу открыты только 22 (allowlist), 80, 443.
- [ ] `docker service ls`: published-порты — ТОЛЬКО 80/443 nginx.
- [ ] С внешней машины: `psql -h <домен> -p 5432` и `redis-cli -h <домен>` НЕ
      подключаются; `curl https://<домен>/metrics` → **403**;
      с клиентским сертификатом (`curl --cert client.crt --key client.key`)
      → **200** (AQ²-10).
- [ ] `docker secret ls`: все 10 секретов из §4 существуют; `git log -p | grep -iE 'api[_-]?key|token|password'` — пусто; `.env` в `.gitignore`.
- [ ] Бэкапы идут: `wal-g backup-list` показывает свежий base-бэкап,
      в бакете появляются WAL-сегменты (или RDS: automated backups on).
- [ ] **DR-drill пройден** (см. §8) — восстановление проверено ДО запуска.
- [ ] `getWebhookInfo`: url = боевой домен, не ngrok.
- [ ] Grafana доступна только через nginx (`/grafana/`), пароль из секрета.
- [ ] `CRYPTOBOT_USE_TESTNET=false`, mainnet-токен на месте.

## 8. Учения (регулярно, не только перед запуском)

**Redis failover (IQ-7)** — локально/на staging:
```bash
./scripts/failover_drill.sh
```
Ожидаемо: pub/sub-обрыв обнаружен мгновенно (< 5 с — Hub уходит в
polling_mode, Kanban живёт), Sentinel промоутит реплику, enqueue оживает за
3–10 с, упавшие enqueue закрывает recovery-cron (§11.2). Прогон 2026-07-06:
обрыв обнаружен за 0 с, очередь ожила за 3–4.1 с, потерь нет.

**PITR (AQ²-8)** — локально (MinIO как S3):
```bash
./scripts/dr_drill.sh
```
На prod тот же порядок руками (раз в квартал): `wal-g backup-fetch` на
чистый том + `recovery_target_time` — цель: восстановление на точку
< 5 мин назад. Для RDS: консоль → Restore to point in time, затем smoke.
Прогон 2026-07-06 (MinIO): восстановление на T1 успешно — маркер до T1
на месте, запись после T1 отсутствует.

**Alerting-smoke:** остановить один Sentinel (`docker service scale
crm_sentinel-3=0`) → в Telegram-чат менеджеров должен прийти
`RedisSentinelDown`; вернуть обратно.

## 9. Эксплуатация

- Обновление: `git pull && docker compose -f docker-compose.prod.yml build && docker stack deploy -c docker-compose.prod.yml crm` (rolling: start-first).
- Дашборды: `https://<домен>/grafana/` → папка Interfin CRM (4 дашборда §14).
- Алерты: Telegram-чат менеджеров (Alertmanager, `ops/alertmanager/`).
- Логи: `docker service logs crm_app --since 1h` (JSON slog, есть `lead_id`).
