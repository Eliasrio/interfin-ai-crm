// payment_smoke — живой smoke-тест M6 против CryptoBot (Crypto Pay API).
//
// Делает: getMe (токен валиден?) → находит/создаёт тестового лида в БД →
// createInvoice с payload=lead_id → печатает платёжную ссылку.
// Дальше вручную: оплатить ссылку в Telegram и смотреть, как вебхук
// двигает лида (README, «Платёжный вебхук CryptoBot локально»).
//
// Запуск (env из .env): go run ./scripts/payment_smoke [amount] [asset]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/shopspring/decimal"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/payment"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// smokeLeadTgID — телеграмный ID синтетического лида для smoke-прогонов.
const smokeLeadTgID = 999000111

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("payment_smoke failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	amount := decimal.NewFromInt(1)
	if len(os.Args) > 1 {
		var err error
		if amount, err = decimal.NewFromString(os.Args[1]); err != nil {
			return fmt.Errorf("amount %q: %w", os.Args[1], err)
		}
	}
	asset := "USDT"
	if len(os.Args) > 2 {
		asset = os.Args[2]
	}

	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "config/config.yaml"
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if !cfg.Payment.UseTestnet {
		// Smoke-скрипт создаёт мусорные инвойсы — в mainnet ему делать нечего.
		return errors.New("payment_smoke работает только в testnet (CRYPTOBOT_USE_TESTNET=true)")
	}
	ctx := context.Background()

	// 1) Живой getMe: токен и сеть валидны.
	me, err := getMe(ctx, cfg.Payment)
	if err != nil {
		return fmt.Errorf("getMe: %w", err)
	}
	log.Info("crypto pay app", "network", "testnet", "app", string(me))

	// 2) Тестовый лид в БД (стадия 2 — «живой», как перед оплатой).
	gdb, err := db.Open(cfg.Database)
	if err != nil {
		return err
	}
	leads, _, _ := repo.New(gdb)
	lead, err := leads.GetByTelegramUserID(ctx, smokeLeadTgID)
	if errors.Is(err, repo.ErrNotFound) {
		name := "Smoke Test Lead (M6)"
		lead = &models.Lead{TelegramUserID: smokeLeadTgID, StageID: 2,
			Name: &name, LastActivityAt: time.Now().UTC()}
		if err := leads.Create(ctx, lead); err != nil {
			return err
		}
		log.Info("создан тестовый лид", "lead_id", lead.ID)
	} else if err != nil {
		return err
	} else {
		log.Info("тестовый лид уже есть", "lead_id", lead.ID, "stage_id", lead.StageID)
	}

	// 3) Живой createInvoice с payload=lead_id.
	client := payment.NewClient(cfg.Payment)
	inv, err := client.CreateInvoice(ctx, lead.ID, payment.CreateInvoiceParams{
		Asset:       asset,
		Amount:      amount,
		Description: fmt.Sprintf("M6 smoke: лид %d", lead.ID),
		ExpiresIn:   3600,
	})
	if err != nil {
		return fmt.Errorf("createInvoice: %w", err)
	}

	fmt.Printf("\nlead_id:     %d (stage %d)\n", lead.ID, lead.StageID)
	fmt.Printf("invoice_id:  %d\n", inv.InvoiceID)
	fmt.Printf("сумма:       %s %s\n", inv.Amount, inv.Asset)
	fmt.Printf("оплатить:    %s\n\n", inv.BotInvoiceURL)
	return nil
}

// getMe — прямой вызов /getMe (метода нет в payment.Client — он нужен только
// этому скрипту как проверка «токен жив»).
func getMe(ctx context.Context, cfg config.PaymentConfig) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		payment.BaseURL(cfg)+"/getMe", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Crypto-Pay-API-Token", cfg.ActiveToken())
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var env struct {
		Ok     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, err
	}
	if !env.Ok {
		return nil, fmt.Errorf("crypto pay: %s (HTTP %d)", env.Error, resp.StatusCode)
	}
	return env.Result, nil
}
