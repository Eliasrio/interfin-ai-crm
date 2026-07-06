# Multi-stage build: компилируем статический бинарь, кладём в маленький alpine.

FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -o /out/migrate ./cmd/migrate

FROM alpine:3.19
RUN adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/server /app/server
COPY --from=build /out/migrate /app/migrate
COPY config/config.yaml /app/config/config.yaml
COPY migrations /app/migrations
USER app
EXPOSE 8080

# §13.2 SRS: healthcheck на /health (wget есть в busybox/alpine)
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:8080/health || exit 1

ENTRYPOINT ["/app/server"]
