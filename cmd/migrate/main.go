// cmd/migrate — обёртка над golang-migrate v4 (SRS §8.6).
//
// Использование (эквивалент CLI из §8.6):
//
//	go run ./cmd/migrate up          # накатить все миграции
//	go run ./cmd/migrate down 1      # откатить одну (проверка обратимости)
//	go run ./cmd/migrate version     # текущая версия схемы
//
// DSN берётся из POSTGRES_DSN, путь к миграциям — из MIGRATIONS_PATH
// (по умолчанию ./migrations). Production: запускать вручную перед deploy,
// НЕ автоматически при старте приложения (§8.6).
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // регистрирует схему pgx5://
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log, os.Args[1:]); err != nil {
		log.Error("migrate failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: migrate <up|down [n]|version>")
	}

	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return errors.New("обязательная переменная POSTGRES_DSN не задана")
	}
	// golang-migrate выбирает драйвер по схеме URL; pgx/v5 зарегистрирован
	// как pgx5:// (сам движок использует simple-protocol-совместимые запросы).
	dsn = strings.Replace(dsn, "postgres://", "pgx5://", 1)
	dsn = strings.Replace(dsn, "postgresql://", "pgx5://", 1)

	path := os.Getenv("MIGRATIONS_PATH")
	if path == "" {
		path = "./migrations"
	}

	m, err := migrate.New("file://"+path, dsn)
	if err != nil {
		return fmt.Errorf("init migrate: %w", err)
	}
	defer func() {
		if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
			log.Warn("migrate close", "source_err", srcErr, "db_err", dbErr)
		}
	}()

	switch args[0] {
	case "up":
		err = m.Up()
	case "down":
		steps := 1
		if len(args) > 1 {
			steps, err = strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("down: некорректное число шагов %q: %w", args[1], err)
			}
		}
		err = m.Steps(-steps)
	case "version":
		v, dirty, vErr := m.Version()
		if vErr != nil {
			if errors.Is(vErr, migrate.ErrNilVersion) {
				log.Info("схема пуста: миграции ещё не применялись")
				return nil
			}
			return fmt.Errorf("version: %w", vErr)
		}
		log.Info("schema version", "version", v, "dirty", dirty)
		return nil
	default:
		return fmt.Errorf("неизвестная команда %q (up|down|version)", args[0])
	}

	if errors.Is(err, migrate.ErrNoChange) {
		log.Info("изменений нет: схема уже актуальна")
		return nil
	}
	if err != nil {
		return err
	}
	log.Info("миграции применены", "command", args[0])
	return nil
}
