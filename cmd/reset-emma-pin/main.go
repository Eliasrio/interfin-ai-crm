// cmd/reset-emma-pin — служебный CLI EP-01 (ТЗ §2.2): сброс забытого PIN
// панели Эммы. Только с сервера — из API PIN не сбрасывается сознательно.
//
// Очищает emma_panel.pin_hash в settings (bootstrap открывается заново) и
// удаляет все PIN-сессии и счётчики брутфорса в Redis. Подтверждение y/N
// со stdin (по образцу cmd/create-manager):
//
//	POSTGRES_DSN=postgres://... REDIS_ADDR=localhost:6379 \
//	    go run ./cmd/reset-emma-pin
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/emma"
	"github.com/interfin/interfin-ai-crm/internal/repo"
	"github.com/interfin/interfin-ai-crm/internal/settings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "reset-emma-pin:", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("не задан POSTGRES_DSN")
	}
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		return fmt.Errorf("не задан REDIS_ADDR (host:port — сессии PIN живут в Redis)")
	}

	fmt.Fprint(os.Stderr, "Сбросить PIN панели Эммы и закрыть все PIN-сессии? [y/N]: ")
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && answer == "" {
		return fmt.Errorf("чтение подтверждения: %w", err)
	}
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		fmt.Println("отменено")
		return nil
	}

	gdb, err := db.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 2})
	if err != nil {
		return err
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: os.Getenv("REDIS_PASSWORD"),
	})
	defer rdb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	purged, err := emma.Reset(ctx,
		settings.New(repo.NewSettings(gdb)), emma.NewRedisStore(rdb))
	if err != nil {
		return err
	}
	fmt.Printf("PIN сброшен: pin_hash очищен, удалено redis-ключей: %d\n", purged)
	fmt.Println("ВАЖНО: реплики приложения увидят сброс в пределах 30 секунд (кэш settings)")
	return nil
}
