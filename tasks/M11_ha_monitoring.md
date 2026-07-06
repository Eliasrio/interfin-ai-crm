# M11 — High Availability + Monitoring

**Ветка:** `feat/m11-ops`
**Зависимости:** вся система (M0–M10)
**Разделы SRS:** §11 (HA), §14 (Monitoring/SLO), §13 (Deploy)

## Цель
Обернуть готовую систему в production-эксплуатацию: отказоустойчивость,
бэкапы, мониторинг, деплой. Последний эпик.

## Задачи
1. **Redis Sentinel** (§11.1): 1 master + 2 replica + 3 Sentinel.
   Asynq через `RedisFailoverClientOpt`. WS Hub pub/sub — тот же Sentinel pool.
2. **Graceful degradation** (§11.2):
   - Redis down → Gin сохраняет inbound, ставит `leads.pending_task=TRUE`
     (поле из §8.1), возвращает 200;
   - recovery cron (30с): `SELECT WHERE pending_task=TRUE` → enqueue при
     восстановлении Redis → `pending_task=FALSE`;
   - WS Hub → polling mode при обрыве pub/sub.
3. **PostgreSQL** (§11.3):
   - streaming replication к read replica;
   - **WAL archiving** → S3 sa-east-1, PITR, RPO ≈ 5 мин (AQ²-8/AQ-8);
   - pgbouncer transaction mode, `default_pool_size = 36`;
   - подтвердить pgx SimpleProtocol работает под нагрузкой (AQ²-3).
4. **Monitoring** (§14):
   - Prometheus метрики (gin_request_duration, asynq_task_duration,
     claude_api_duration, ws_event_latency, asynq_queue{dead});
   - **`/metrics` защита**: Nginx mTLS + IP allowlist (`metrics_ip_allowlist`),
     чужой IP → 403 (AQ²-10);
   - Alertmanager rules: WebhookHighLatency, DeadLetterQueueGrowing,
     RedisSentinelDown;
   - Grafana dashboards: CRM Overview, Worker Health, AI Performance,
     Infrastructure.
5. **Deploy** (§13): `docker-compose.prod.yml`, Docker Swarm stack, Docker
   secrets, HEALTHCHECK. Матрица окружений dev/staging/prod.

6. **Провижининг сервера (боевой хостинг).**
   Рекомендация по умолчанию: **AWS, регион sa-east-1 (São Paulo)** — данные
   бразильских клиентов остаются в Бразилии (LGPD), тот же регион, что для S3.
   Альтернатива при экономии: Hetzner/DigitalOcean (данные в ЕС).
   - Целевая конфигурация старта: ~4 vCPU / 8–16 ГБ RAM (VPS или EC2).
   - Управляемый PostgreSQL (AWS RDS) вместо самостоятельного — снимает заботу
     о репликации и WAL-бэкапах; либо self-hosted по §11.3, если бюджет важнее.
   - Установить Docker + Docker Swarm на сервере.
   - Секреты — через Docker secrets (не в файлах, не в git).
   - Nginx как reverse-proxy: TLS (Let's Encrypt/Certbot), проксирование на
     бэкенд, защита `/metrics` (mTLS + IP allowlist).
   - Telegram webhook_url указывает на боевой домен с валидным TLS.
   - **Чек-лист безопасности перед запуском** (обязательно):
     - firewall: наружу открыты только 443 (и 80 для редиректа); БД/Redis/metrics
       НЕ доступны из интернета;
     - все секреты в Docker secrets, ключей нет в коде и коммитах;
     - бэкапы PostgreSQL идут и проверены восстановлением (DR drill);
     - `.env`/секреты не попали в git-историю.

> Первый деплой разумно доверить DevOps-специалисту разово (несколько часов):
> все конфиги готовит Claude Code (задачи выше), человек накатывает и проверяет
> чек-лист безопасности. Дальнейшую эксплуатацию ведёте вы с Claude Code.

## Контракт наружу
- Production-ready система на боевом сервере с SLO-мониторингом,
  отказоустойчивостью и закрытым периметром безопасности.

## Критерии приёмки
- [ ] Redis failover → Kanban продолжает работу (polling fallback) < 5с (IQ-7).
- [ ] Redis down → сообщения не теряются, recovery восстанавливает очередь.
- [ ] WAL: PITR-recovery до точки < 5 мин назад (AQ²-8 DR drill).
- [ ] `/metrics` с чужого IP → 403, с mTLS → 200 (AQ²-10 security test).
- [ ] Под нагрузкой 0 ошибок "prepared statement does not exist" (AQ²-3 load test).
- [ ] Все SLO-метрики видны в Grafana, алерты срабатывают.
- [ ] Система развёрнута на боевом сервере, домен с валидным TLS работает.
- [ ] Чек-лист безопасности пройден: firewall закрыт, секреты в Docker secrets,
      БД/Redis/metrics недоступны из интернета, бэкапы проверены.
- [ ] Telegram-бот отвечает через боевой webhook (не ngrok).
