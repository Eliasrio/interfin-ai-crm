// Package config загружает config.yaml (SRS §12) с подстановкой ${ENV_VAR}.
//
// Правила (CLAUDE.md §4.9): секреты только через окружение. Если обязательная
// переменная не задана — явная ошибка на старте, без тихих пустых значений.
package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/viper"
)

// Config — корневой объект конфигурации, доступный всем модулям (контракт M0).
type Config struct {
	Server     ServerConfig     `mapstructure:"server"`
	Telegram   TelegramConfig   `mapstructure:"telegram"`
	Database   DatabaseConfig   `mapstructure:"database"`
	Redis      RedisConfig      `mapstructure:"redis"`
	Auth       AuthConfig       `mapstructure:"auth"`
	Claude     ClaudeConfig     `mapstructure:"claude"`
	Embeddings EmbeddingsConfig `mapstructure:"embeddings"`
	AWS        AWSConfig        `mapstructure:"aws"`
	Kanban     KanbanConfig     `mapstructure:"kanban"`
	Payment    PaymentConfig    `mapstructure:"payment"`
	RAG        RAGConfig        `mapstructure:"rag"`
	LGPD       LGPDConfig       `mapstructure:"lgpd"`
	Monitoring MonitoringConfig `mapstructure:"monitoring"`
}

type ServerConfig struct {
	Port int `mapstructure:"port"`
}

type TelegramConfig struct {
	BotToken      string `mapstructure:"bot_token"`
	WebhookSecret string `mapstructure:"webhook_secret"`
	WebhookURL    string `mapstructure:"webhook_url"`
	// ManagerChatID — чат для алертов dead letter (§6.3). Опционален:
	// 0 = алерты остаются только в логе.
	ManagerChatID int64 `mapstructure:"manager_chat_id"`
}

type DatabaseConfig struct {
	DSN           string `mapstructure:"dsn"`
	MaxOpenConns  int    `mapstructure:"max_open_conns"`
	PrepareStmt   bool   `mapstructure:"prepare_stmt"`    // AQ²-fix #3: всегда false
	QueryExecMode string `mapstructure:"query_exec_mode"` // AQ²-fix #3: всегда "simple"
}

type RedisConfig struct {
	Addr          string   `mapstructure:"addr"` // dev/staging: одиночный Redis
	SentinelAddrs []string `mapstructure:"sentinel_addrs"`
	MasterName    string   `mapstructure:"master_name"`
	Password      string   `mapstructure:"password"`
}

type AuthConfig struct {
	JWTPrivateKeyPath string `mapstructure:"jwt_private_key_path"`
	JWTPublicKeyPath  string `mapstructure:"jwt_public_key_path"`
	AccessTokenTTL    int    `mapstructure:"access_token_ttl"`  // секунды
	RefreshTokenTTL   int    `mapstructure:"refresh_token_ttl"` // секунды
}

type TokenBudget struct {
	SystemPrompt int `mapstructure:"system_prompt"`
	Summary      int `mapstructure:"summary"`
	History      int `mapstructure:"history"`
	SafetyBuffer int `mapstructure:"safety_buffer"`
}

type ClaudeConfig struct {
	APIKey               string      `mapstructure:"api_key"`
	Model                string      `mapstructure:"model"`
	ClaudeReplyTokens    int         `mapstructure:"claude_reply_tokens"`
	TokenBudget          TokenBudget `mapstructure:"token_budget"`
	CountTokensThreshold int         `mapstructure:"count_tokens_threshold"` // AQ²-fix #7
}

type EmbeddingsConfig struct { // AQ²-fix #2: Voyage, не OpenAI
	Provider   string `mapstructure:"provider"`
	APIKey     string `mapstructure:"api_key"`
	Model      string `mapstructure:"model"`
	Dimensions int    `mapstructure:"dimensions"`
}

type AWSConfig struct {
	Region    string `mapstructure:"region"`
	Bucket    string `mapstructure:"bucket"`
	AccessKey string `mapstructure:"access_key"`
	SecretKey string `mapstructure:"secret_key"`
}

type KanbanConfig struct {
	AntiSpamLimit         int     `mapstructure:"anti_spam_limit"`
	AntiSpamFollowupHours int     `mapstructure:"anti_spam_followup_hours"` // AQ²-fix #8
	AntiSpamEscalateHours int     `mapstructure:"anti_spam_escalate_hours"` // AQ²-fix #8
	TTLStage4Hours        int     `mapstructure:"ttl_stage4_hours"`
	TTLStage6Days         int     `mapstructure:"ttl_stage6_days"`
	UnderpaidTolerancePct float64 `mapstructure:"underpaid_tolerance_pct"`
	SummaryEveryNMessages int     `mapstructure:"summary_every_n_messages"`
}

// PaymentConfig — крипто-шлюз CryptoBot / Crypto Pay API (M6, §3.3/§5.5).
// Сеть переключается use_testnet: true — testnet (разработка), false —
// mainnet (боевой). HMAC-ключ вебхука производный от активного токена
// (SHA256(token), спецификация Crypto Pay) — см. internal/payment.
type PaymentConfig struct {
	Gateway             string `mapstructure:"gateway"`       // всегда "cryptobot"
	TestnetToken        string `mapstructure:"testnet_token"` // CRYPTOBOT_TESTNET_TOKEN
	MainnetToken        string `mapstructure:"mainnet_token"` // CRYPTOBOT_MAINNET_TOKEN
	UseTestnet          bool   `mapstructure:"use_testnet"`   // CRYPTOBOT_USE_TESTNET
	ReplayWindowMinutes int    `mapstructure:"replay_window_minutes"` // §5.5: окно ±5 мин
}

// ActiveToken — токен приложения выбранной сети; материал HMAC-ключа вебхука
// и аутентификация Crypto Pay API.
func (p PaymentConfig) ActiveToken() string {
	if p.UseTestnet {
		return p.TestnetToken
	}
	return p.MainnetToken
}

// configured — секция payment заполнена (в юнит-тестовых yaml её может
// не быть вовсе — платёжный вебхук там не собирается).
func (p PaymentConfig) configured() bool {
	return p.Gateway != "" || p.TestnetToken != "" || p.MainnetToken != ""
}

type RAGConfig struct {
	CosineThreshold float64 `mapstructure:"cosine_threshold"`
	TopK            int     `mapstructure:"top_k"`
	FallbackOnMiss  bool    `mapstructure:"fallback_on_miss"`
}

type LGPDConfig struct {
	RetentionDays            int      `mapstructure:"retention_days"`
	PreserveFinancialRecords bool     `mapstructure:"preserve_financial_records"` // AQ²-fix #4
	ErasureAnonymizeFields   []string `mapstructure:"erasure_anonymize_fields"`
	ErasureHashFields        []string `mapstructure:"erasure_hash_fields"` // AQ²-fix #4
}

type MonitoringConfig struct {
	PrometheusPort     int      `mapstructure:"prometheus_port"`
	MetricsIPAllowlist []string `mapstructure:"metrics_ip_allowlist"` // AQ²-fix #10
	AlertmanagerURL    string   `mapstructure:"alertmanager_url"`
}

// requiredEnv — переменные, без которых процесс не имеет права стартовать.
// Список расширяется по мере эпиков.
var requiredEnv = []string{
	"POSTGRES_DSN",
	"REDIS_ADDR",
	// M2 (Telegram ingestion): без токена бот не создаётся, без секрета
	// webhook отвечал бы 403 всем (§5.4), без URL некуда делать setWebhook.
	"TELEGRAM_BOT_TOKEN",
	"TELEGRAM_WEBHOOK_SECRET",
	"TELEGRAM_WEBHOOK_URL",
	// M3 (воркер + Claude): без ключа воркер не может звать /v1/messages.
	"ANTHROPIC_API_KEY",
	// M4 (RAG): эмбеддинги Voyage voyage-3 (AQ²-fix #2 — НЕ OpenAI).
	"VOYAGE_API_KEY",
	// M6 (payment): токены CryptoBot обеих сетей — без активного токена
	// вебхук отвечал бы 403 всем (§5.5), а тихо пустой токен другой сети
	// всплыл бы только при переключении use_testnet в бою.
	"CRYPTOBOT_TESTNET_TOKEN",
	"CRYPTOBOT_MAINNET_TOKEN",
}

// defaultEnv — значения для незаданных НЕобязательных переменных.
var defaultEnv = map[string]string{
	"HTTP_PORT": "8080", // §13.2: healthcheck ходит на :8080
	// M6: не задан — работаем в testnet; боевой режим включается только
	// явным CRYPTOBOT_USE_TESTNET=false (безопасный дефолт для dev).
	"CRYPTOBOT_USE_TESTNET": "true",
}

var envPlaceholder = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load читает YAML по пути path, подставляет ${ENV_VAR} и валидирует результат.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	expanded, missing := expandEnv(string(raw))
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf(
			"config: обязательные переменные окружения не заданы: %s",
			strings.Join(missing, ", "))
	}

	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(bytes.NewReader([]byte(expanded))); err != nil {
		return nil, fmt.Errorf("config: parse yaml: %w", err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// expandEnv подставляет ${VAR}: значение из окружения, иначе default,
// иначе пустая строка + пометка в missing, если переменная обязательная.
func expandEnv(src string) (string, []string) {
	missingSet := map[string]bool{}
	out := envPlaceholder.ReplaceAllStringFunc(src, func(match string) string {
		name := envPlaceholder.FindStringSubmatch(match)[1]
		if val, ok := os.LookupEnv(name); ok {
			return val
		}
		if def, ok := defaultEnv[name]; ok {
			return def
		}
		if isRequired(name) {
			missingSet[name] = true
		}
		return ""
	})

	missing := make([]string, 0, len(missingSet))
	for name := range missingSet {
		missing = append(missing, name)
	}
	return out, missing
}

func isRequired(name string) bool {
	for _, r := range requiredEnv {
		if r == name {
			return true
		}
	}
	return false
}

// validate — страховка от пустых обязательных значений (например, переменная
// задана, но пустой строкой) и от нарушения жёстких правил CLAUDE.md §4.
func (c *Config) validate() error {
	var problems []string

	if c.Database.DSN == "" {
		problems = append(problems, "database.dsn пуст (POSTGRES_DSN)")
	}
	if c.Redis.Addr == "" && len(c.Redis.SentinelAddrs) == 0 {
		problems = append(problems, "redis: не задан ни addr (REDIS_ADDR), ни sentinel_addrs")
	}
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		problems = append(problems, fmt.Sprintf("server.port вне диапазона: %d", c.Server.Port))
	}
	// AQ²-fix #3: pgbouncer transaction mode ломает prepared statements.
	if c.Database.PrepareStmt {
		problems = append(problems, "database.prepare_stmt должен быть false (AQ²-fix #3)")
	}
	if c.Database.QueryExecMode != "simple" {
		problems = append(problems, "database.query_exec_mode должен быть \"simple\" (AQ²-fix #3)")
	}
	// M2: telegram-секция либо не заполнена вовсе (yaml без неё — например,
	// в юнит-тестах), либо заполнена целиком: частичная конфигурация — это
	// бот без секрета или без URL, оба варианта небезопасны/неработоспособны.
	tg := c.Telegram
	if (tg.BotToken != "" || tg.WebhookSecret != "" || tg.WebhookURL != "") &&
		(tg.BotToken == "" || tg.WebhookSecret == "" || tg.WebhookURL == "") {
		problems = append(problems,
			"telegram: bot_token, webhook_secret и webhook_url задаются только вместе (§5.4, §6.1)")
	}

	// M3: claude-секция проверяется, когда задан api_key (в юнит-тестовых
	// yaml без ключа секция может быть частичной — воркер там не собирается).
	if cl := c.Claude; cl.APIKey != "" {
		b := cl.TokenBudget
		switch {
		case cl.Model == "":
			problems = append(problems, "claude.model пуст")
		case cl.ClaudeReplyTokens <= 0:
			problems = append(problems, "claude.claude_reply_tokens должен быть > 0 (§7.2)")
		case b.SystemPrompt <= 0 || b.History <= 0 || b.Summary < 0 || b.SafetyBuffer < 0:
			problems = append(problems, "claude.token_budget: компоненты бюджета невалидны (§7.2)")
		case b.SystemPrompt+b.Summary+b.History+b.SafetyBuffer+cl.ClaudeReplyTokens > 9000:
			// IQ-6: запрос к Claude никогда не превышает 9000 токенов.
			problems = append(problems, "claude: бюджет + claude_reply_tokens превышают 9000 (IQ-6)")
		case cl.CountTokensThreshold <= 0:
			problems = append(problems, "claude.count_tokens_threshold должен быть > 0 (AQ²-fix #7)")
		}
	}

	// M4: embeddings-секция проверяется, когда задан api_key (юнит-тестовые
	// yaml без ключа могут не заполнять её — RAG там не собирается).
	if em := c.Embeddings; em.APIKey != "" {
		switch {
		case em.Provider != "voyage":
			// AQ²-fix #2: OPENAI_API_KEY нигде не требуется.
			problems = append(problems, fmt.Sprintf(
				"embeddings.provider должен быть \"voyage\", не %q (AQ²-2)", em.Provider))
		case em.Model == "":
			problems = append(problems, "embeddings.model пуст")
		case em.Dimensions != 1024:
			// §7.1: pgvector-колонка vector(1024) — иная размерность не запишется.
			problems = append(problems, fmt.Sprintf(
				"embeddings.dimensions должен быть 1024 (§7.1), не %d", em.Dimensions))
		}
		// RAG-параметры (§7.1): порог и top_k читаются отсюда, не из констант.
		if r := c.RAG; r.CosineThreshold <= 0 || r.CosineThreshold >= 1 {
			problems = append(problems, fmt.Sprintf(
				"rag.cosine_threshold вне (0,1): %v", r.CosineThreshold))
		} else if r.TopK <= 0 {
			problems = append(problems, fmt.Sprintf("rag.top_k должен быть > 0: %d", r.TopK))
		}
	}

	// M6: payment-секция проверяется, когда заполнена (юнит-тестовые yaml
	// без неё валидны — платёжный вебхук там не собирается).
	if p := c.Payment; p.configured() {
		switch {
		case p.Gateway != "cryptobot":
			problems = append(problems, fmt.Sprintf(
				"payment.gateway должен быть \"cryptobot\", не %q (M6)", p.Gateway))
		case p.ActiveToken() == "":
			problems = append(problems, fmt.Sprintf(
				"payment: токен активной сети пуст (use_testnet=%v → %s)",
				p.UseTestnet, map[bool]string{true: "CRYPTOBOT_TESTNET_TOKEN", false: "CRYPTOBOT_MAINNET_TOKEN"}[p.UseTestnet]))
		case p.ReplayWindowMinutes <= 0:
			problems = append(problems, "payment.replay_window_minutes должен быть > 0 (§5.5)")
		}
		// §3.3: tolerance читается из конфига (задача M6), 0 или 100+ — бессмыслица.
		if pct := c.Kanban.UnderpaidTolerancePct; pct <= 0 || pct >= 100 {
			problems = append(problems, fmt.Sprintf(
				"kanban.underpaid_tolerance_pct вне (0,100): %v (§3.3)", pct))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("config: невалидная конфигурация: %s", strings.Join(problems, "; "))
	}
	return nil
}
