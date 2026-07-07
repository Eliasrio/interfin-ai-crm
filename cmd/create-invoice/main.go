// cmd/create-invoice — служебный CLI: выставляет лиду инвойс CryptoBot
// (Crypto Pay createInvoice, §3.3) и печатает платёжную ссылку для клиента.
// Оплата ссылки двигает лида по доске автоматически (вебхук M6).
//
// До появления кнопки на доске это единственный боевой способ выставить
// счёт; на проде запускается one-off в контейнере app (scripts/new_invoice.sh).
//
// Env (как у cmd/index-kb — без полного config.yaml): POSTGRES_DSN,
// CRYPTOBOT_TESTNET_TOKEN, CRYPTOBOT_MAINNET_TOKEN, CRYPTOBOT_USE_TESTNET
// (не задан → true, testnet — безопасный дефолт, как в config.Load).
//
//	go run ./cmd/create-invoice -lead 1 -amount 2000 -asset USDT
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/shopspring/decimal"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/payment"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "create-invoice:", err)
		os.Exit(1)
	}
}

func run() error {
	leadID := flag.Int64("lead", 0, "ID лида (колонка id на доске, обязателен)")
	amountStr := flag.String("amount", "", "сумма счёта, например 2000 (обязательна)")
	asset := flag.String("asset", "USDT", "валюта: USDT | USDC (§3.3)")
	desc := flag.String("desc", "", "описание в счёте (видит клиент; по умолчанию — имя услуги не подставляется)")
	expiresHours := flag.Int("expires-hours", 24, "срок жизни счёта в часах")
	flag.Parse()

	if *leadID <= 0 {
		return errors.New("флаг -lead обязателен (ID лида с доски)")
	}
	amount, err := decimal.NewFromString(*amountStr)
	if err != nil || !amount.IsPositive() {
		return fmt.Errorf("флаг -amount обязателен и должен быть положительным числом, получено %q", *amountStr)
	}

	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return errors.New("обязательная переменная POSTGRES_DSN не задана")
	}
	// Дефолт testnet=true повторяет config.Load (config.go): забытый env
	// не должен молча выставлять счета в боевой сети.
	pcfg := config.PaymentConfig{
		TestnetToken: os.Getenv("CRYPTOBOT_TESTNET_TOKEN"),
		MainnetToken: os.Getenv("CRYPTOBOT_MAINNET_TOKEN"),
		UseTestnet:   os.Getenv("CRYPTOBOT_USE_TESTNET") != "false",
	}
	if pcfg.ActiveToken() == "" {
		net := map[bool]string{true: "CRYPTOBOT_TESTNET_TOKEN", false: "CRYPTOBOT_MAINNET_TOKEN"}[pcfg.UseTestnet]
		return fmt.Errorf("токен активной сети пуст (use_testnet=%v → %s)", pcfg.UseTestnet, net)
	}

	gormDB, err := db.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 2})
	if err != nil {
		return err
	}
	sqlDB, err := gormDB.DB()
	if err != nil {
		return fmt.Errorf("gorm sql db: %w", err)
	}
	defer sqlDB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Лид обязан существовать и быть живым: вебхук свяжет оплату по
	// invoice.payload=lead_id, счёт «в никуда» потерял бы платёж.
	leads, _, _ := repo.New(gormDB)
	lead, err := leads.GetByID(ctx, *leadID)
	if errors.Is(err, repo.ErrNotFound) {
		return fmt.Errorf("лид %d не найден (или стёрт по LGPD) — проверьте ID на доске", *leadID)
	} else if err != nil {
		return err
	}

	inv, err := payment.NewClient(pcfg).CreateInvoice(ctx, lead.ID, payment.CreateInvoiceParams{
		Asset:       *asset,
		Amount:      amount,
		Description: *desc,
		ExpiresIn:   *expiresHours * 3600,
	})
	if err != nil {
		return fmt.Errorf("createInvoice: %w", err)
	}

	name := ""
	if lead.Name != nil {
		name = " (" + *lead.Name + ")"
	}
	network := map[bool]string{true: "TESTNET (тестовая сеть, не настоящие деньги!)", false: "mainnet (боевая сеть)"}[pcfg.UseTestnet]
	fmt.Printf("\nСеть:        %s\n", network)
	fmt.Printf("Лид:         %d%s, стадия %d\n", lead.ID, name, lead.StageID)
	fmt.Printf("Счёт:        №%d на %s %s, действует %d ч\n", inv.InvoiceID, inv.Amount, inv.Asset, *expiresHours)
	fmt.Printf("Ссылка для клиента:\n%s\n\n", inv.BotInvoiceURL)
	return nil
}
