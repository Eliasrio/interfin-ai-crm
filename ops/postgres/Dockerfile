# ops/postgres — образ PostgreSQL 16 + pgvector + wal-g (M11 §11.3).
#
# wal-g гонит WAL-сегменты и base-бэкапы в S3 (sa-east-1) — основа PITR
# c RPO ≈ 5 мин (AQ²-8). Тот же образ используется primary и replica
# (роль решает команда/entrypoint в stack-файле).

FROM pgvector/pgvector:pg16

# Свежие корневые сертификаты: комплект в базовом образе устаревает, и
# wal-g перестаёт доверять Backblaze («x509: certificate signed by unknown
# authority»), причём AWS SDK такие ошибки МОЛЧА ретраит с backoff'ом —
# выглядит как вечное зависание backup-push (поймано при первом боевом
# деплое).
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && update-ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# В релизе v3.0.3 бинари собраны под ubuntu-20.04 (glibc совместим с debian
# bookworm базового образа).
ARG WALG_VERSION=v3.0.3
ADD https://github.com/wal-g/wal-g/releases/download/${WALG_VERSION}/wal-g-pg-ubuntu-20.04-amd64.tar.gz /tmp/walg.tar.gz
RUN tar -xzf /tmp/walg.tar.gz -C /usr/local/bin \
 && mv /usr/local/bin/wal-g-pg-ubuntu-20.04-amd64 /usr/local/bin/wal-g \
 && chmod +x /usr/local/bin/wal-g \
 && rm /tmp/walg.tar.gz

# Мост «Docker secrets → env» для wal-g (AWS-ключи, WALG_S3_PREFIX):
# секрет walg_env — файл KEY=VALUE, как app_env у приложения.
COPY walg-entrypoint.sh /usr/local/bin/walg-entrypoint.sh
RUN chmod +x /usr/local/bin/walg-entrypoint.sh

ENTRYPOINT ["/usr/local/bin/walg-entrypoint.sh"]
CMD ["postgres"]
