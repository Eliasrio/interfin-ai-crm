package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testYAML = `
server:
  port: ${HTTP_PORT}
database:
  dsn: ${POSTGRES_DSN}
  max_open_conns: 36
  prepare_stmt: false
  query_exec_mode: simple
redis:
  addr: ${REDIS_ADDR}
  password: ${REDIS_PASSWORD}
claude:
  model: claude-3-5-sonnet-20241022
`

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoad_SubstitutesEnvVars(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_ADDR", "localhost:6379")
	t.Setenv("HTTP_PORT", "9999")

	cfg, err := Load(writeTempConfig(t, testYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.DSN != "postgres://u:p@localhost:5432/db" {
		t.Errorf("database.dsn = %q, подстановка POSTGRES_DSN не сработала", cfg.Database.DSN)
	}
	if cfg.Redis.Addr != "localhost:6379" {
		t.Errorf("redis.addr = %q, подстановка REDIS_ADDR не сработала", cfg.Redis.Addr)
	}
	if cfg.Server.Port != 9999 {
		t.Errorf("server.port = %d, ожидали 9999", cfg.Server.Port)
	}
	if cfg.Claude.Model != "claude-3-5-sonnet-20241022" {
		t.Errorf("claude.model = %q", cfg.Claude.Model)
	}
}

func TestLoad_MissingRequiredEnvFails(t *testing.T) {
	// POSTGRES_DSN и REDIS_ADDR намеренно не заданы.
	os.Unsetenv("POSTGRES_DSN")
	os.Unsetenv("REDIS_ADDR")

	_, err := Load(writeTempConfig(t, testYAML))
	if err == nil {
		t.Fatal("Load должен вернуть ошибку при незаданных обязательных переменных")
	}
	if !strings.Contains(err.Error(), "POSTGRES_DSN") || !strings.Contains(err.Error(), "REDIS_ADDR") {
		t.Errorf("ошибка должна называть недостающие переменные, получили: %v", err)
	}
}

func TestLoad_EmptyRequiredEnvFails(t *testing.T) {
	// Переменная задана, но пустая — тоже ошибка (validate).
	t.Setenv("POSTGRES_DSN", "")
	t.Setenv("REDIS_ADDR", "localhost:6379")

	_, err := Load(writeTempConfig(t, testYAML))
	if err == nil {
		t.Fatal("Load должен вернуть ошибку при пустом POSTGRES_DSN")
	}
}

func TestLoad_OptionalEnvDefaults(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_ADDR", "localhost:6379")
	os.Unsetenv("HTTP_PORT") // не задан → default 8080 (§13.2)

	cfg, err := Load(writeTempConfig(t, testYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("server.port = %d, ожидали default 8080", cfg.Server.Port)
	}
}

const telegramYAML = testYAML + `
telegram:
  bot_token: ${TELEGRAM_BOT_TOKEN}
  webhook_secret: ${TELEGRAM_WEBHOOK_SECRET}
  webhook_url: ${TELEGRAM_WEBHOOK_URL}
`

func TestLoad_TelegramEnvRequired(t *testing.T) {
	// M2: конфиг с telegram-секцией обязан требовать все три переменные.
	t.Setenv("POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_ADDR", "localhost:6379")
	os.Unsetenv("TELEGRAM_BOT_TOKEN")
	os.Unsetenv("TELEGRAM_WEBHOOK_SECRET")
	os.Unsetenv("TELEGRAM_WEBHOOK_URL")

	_, err := Load(writeTempConfig(t, telegramYAML))
	if err == nil {
		t.Fatal("Load должен требовать TELEGRAM_* переменные для конфига с telegram-секцией")
	}
	for _, name := range []string{"TELEGRAM_BOT_TOKEN", "TELEGRAM_WEBHOOK_SECRET", "TELEGRAM_WEBHOOK_URL"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("ошибка должна называть %s, получили: %v", name, err)
		}
	}
}

func TestLoad_TelegramComplete(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_ADDR", "localhost:6379")
	t.Setenv("TELEGRAM_BOT_TOKEN", "123:token")
	t.Setenv("TELEGRAM_WEBHOOK_SECRET", "s3cret")
	t.Setenv("TELEGRAM_WEBHOOK_URL", "https://crm.example.org/webhook/telegram")

	cfg, err := Load(writeTempConfig(t, telegramYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Telegram.BotToken != "123:token" ||
		cfg.Telegram.WebhookSecret != "s3cret" ||
		cfg.Telegram.WebhookURL != "https://crm.example.org/webhook/telegram" {
		t.Errorf("telegram-конфиг собран неверно: %+v", cfg.Telegram)
	}
}

func TestLoad_TelegramPartialFails(t *testing.T) {
	// Частичная конфигурация = бот без секрета (§5.4) или без URL — ошибка.
	t.Setenv("POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_ADDR", "localhost:6379")

	partial := testYAML + `
telegram:
  bot_token: 123:token
  webhook_secret: ""
  webhook_url: https://crm.example.org/webhook/telegram
`
	if _, err := Load(writeTempConfig(t, partial)); err == nil {
		t.Fatal("бот с пустым webhook_secret должен отвергаться на старте")
	}
}

// --- M6: payment (CryptoBot) ---

const paymentYAML = testYAML + `
kanban:
  underpaid_tolerance_pct: 2.0
payment:
  gateway: cryptobot
  use_testnet: ${CRYPTOBOT_USE_TESTNET}
  testnet_token: ${CRYPTOBOT_TESTNET_TOKEN}
  mainnet_token: ${CRYPTOBOT_MAINNET_TOKEN}
  replay_window_minutes: 5
`

func setPaymentEnv(t *testing.T) {
	t.Helper()
	t.Setenv("POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_ADDR", "localhost:6379")
	t.Setenv("CRYPTOBOT_TESTNET_TOKEN", "111:testnet")
	t.Setenv("CRYPTOBOT_MAINNET_TOKEN", "222:mainnet")
}

func TestLoad_PaymentEnvRequired(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_ADDR", "localhost:6379")
	os.Unsetenv("CRYPTOBOT_TESTNET_TOKEN")
	os.Unsetenv("CRYPTOBOT_MAINNET_TOKEN")

	_, err := Load(writeTempConfig(t, paymentYAML))
	if err == nil {
		t.Fatal("Load должен требовать CRYPTOBOT_* токены для конфига с payment-секцией")
	}
	for _, name := range []string{"CRYPTOBOT_TESTNET_TOKEN", "CRYPTOBOT_MAINNET_TOKEN"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("ошибка должна называть %s, получили: %v", name, err)
		}
	}
}

func TestLoad_PaymentTestnetDefault(t *testing.T) {
	// CRYPTOBOT_USE_TESTNET не задан → безопасный дефолт true (testnet):
	// боевой режим включается только явным false.
	setPaymentEnv(t)
	os.Unsetenv("CRYPTOBOT_USE_TESTNET")

	cfg, err := Load(writeTempConfig(t, paymentYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Payment.UseTestnet {
		t.Error("незаданный CRYPTOBOT_USE_TESTNET должен давать testnet (безопасный дефолт)")
	}
	if cfg.Payment.ActiveToken() != "111:testnet" {
		t.Errorf("ActiveToken = %q, ожидали testnet-токен", cfg.Payment.ActiveToken())
	}
}

func TestLoad_PaymentMainnetSwitch(t *testing.T) {
	setPaymentEnv(t)
	t.Setenv("CRYPTOBOT_USE_TESTNET", "false")

	cfg, err := Load(writeTempConfig(t, paymentYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Payment.UseTestnet {
		t.Error("CRYPTOBOT_USE_TESTNET=false должен включать mainnet")
	}
	if cfg.Payment.ActiveToken() != "222:mainnet" {
		t.Errorf("ActiveToken = %q, ожидали mainnet-токен", cfg.Payment.ActiveToken())
	}
}

func TestLoad_PaymentBadTolerance(t *testing.T) {
	// §3.3: tolerance из конфига; 0 и 100+ — ошибки конфигурации.
	setPaymentEnv(t)
	bad := strings.Replace(paymentYAML, "underpaid_tolerance_pct: 2.0",
		"underpaid_tolerance_pct: 0", 1)
	if _, err := Load(writeTempConfig(t, bad)); err == nil {
		t.Fatal("нулевой underpaid_tolerance_pct при настроенном payment должен отвергаться")
	}
}

func TestLoad_PaymentWrongGateway(t *testing.T) {
	setPaymentEnv(t)
	bad := strings.Replace(paymentYAML, "gateway: cryptobot", "gateway: stripe", 1)
	if _, err := Load(writeTempConfig(t, bad)); err == nil {
		t.Fatal("незнакомый payment.gateway должен отвергаться")
	}
}

func TestLoad_RejectsPreparedStatements(t *testing.T) {
	// AQ²-fix #3: prepare_stmt=true — это баг конфигурации, ловим на старте.
	t.Setenv("POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_ADDR", "localhost:6379")

	bad := strings.Replace(testYAML, "prepare_stmt: false", "prepare_stmt: true", 1)
	if _, err := Load(writeTempConfig(t, bad)); err == nil {
		t.Fatal("Load должен отклонять prepare_stmt: true (AQ²-fix #3)")
	}
}

// M11 §11.1: REDIS_SENTINEL_ADDRS — строка с запятыми, viper разворачивает
// её в []string (StringToSliceHookFunc). Задана → failover-режим (addr может
// быть пустым); не задана → одиночный addr обязателен.
func TestLoad_SentinelAddrsFromEnv(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_SENTINEL_ADDRS", "sentinel-1:26379,sentinel-2:26379,sentinel-3:26379")

	yaml := `
server:
  port: 8080
database:
  dsn: ${POSTGRES_DSN}
  prepare_stmt: false
  query_exec_mode: simple
redis:
  addr: ""
  sentinel_addrs: ${REDIS_SENTINEL_ADDRS}
  master_name: mymaster
`
	cfg, err := Load(writeTempConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"sentinel-1:26379", "sentinel-2:26379", "sentinel-3:26379"}
	if len(cfg.Redis.SentinelAddrs) != len(want) {
		t.Fatalf("sentinel_addrs = %q, ожидали %q", cfg.Redis.SentinelAddrs, want)
	}
	for i := range want {
		if cfg.Redis.SentinelAddrs[i] != want[i] {
			t.Errorf("sentinel_addrs[%d] = %q, ожидали %q", i, cfg.Redis.SentinelAddrs[i], want[i])
		}
	}
	if cfg.Redis.MasterName != "mymaster" {
		t.Errorf("master_name = %q", cfg.Redis.MasterName)
	}
}

func TestLoad_NoRedisAtAllFails(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "postgres://u:p@localhost:5432/db")

	yaml := `
server:
  port: 8080
database:
  dsn: ${POSTGRES_DSN}
  prepare_stmt: false
  query_exec_mode: simple
redis:
  addr: ""
  sentinel_addrs: []
`
	if _, err := Load(writeTempConfig(t, yaml)); err == nil {
		t.Fatal("конфиг без addr и без sentinel_addrs обязан быть ошибкой")
	}
}
