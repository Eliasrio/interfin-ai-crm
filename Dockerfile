# Multi-stage build: компилируем статический бинарь, кладём в маленький alpine.

# React-доска (M10): собирается ЗДЕСЬ, а не берётся с диска — web/dist в
# git не живёт, и без этого этапа боевой образ отдавал 404 на / (поймано
# при первом боевом деплое).
FROM node:20-alpine AS webbuild
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -o /out/migrate ./cmd/migrate \
 && CGO_ENABLED=0 go build -o /out/create-manager ./cmd/create-manager \
 && CGO_ENABLED=0 go build -o /out/index-kb ./cmd/index-kb

# 3.21 вместо 3.19: нужен поддерживаемый репозиторий со свежим
# ca-certificates — устаревший комплект корней даёт «x509: unknown
# authority» на новых цепочках (Backblaze/Let's Encrypt), а Go-клиенты
# (Claude/Voyage/CryptoBot) такие ошибки молча ретраят (урок первого
# боевого деплоя — см. ops/postgres/Dockerfile).
FROM alpine:3.21
RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/server /app/server
COPY --from=build /out/migrate /app/migrate
# M11: ops-утилиты для боевого сервера (one-off запуск на сети стека):
# бутстрап учётки менеджера и индексация базы знаний RAG.
COPY --from=build /out/create-manager /app/create-manager
COPY --from=build /out/index-kb /app/index-kb
COPY config/config.yaml /app/config/config.yaml
COPY migrations /app/migrations
# server.static_dir: web/dist (относительно WORKDIR /app)
COPY --from=webbuild /web/dist /app/web/dist
# M11: мост «Docker secrets → env» для prod (dev без секрета app_env — no-op).
COPY ops/docker/app-entrypoint.sh /app/entrypoint.sh
USER app
EXPOSE 8080

# §13.2 SRS: healthcheck на /health (wget есть в busybox/alpine)
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:8080/health || exit 1

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["/app/server"]
