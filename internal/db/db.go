// Package db открывает подключение к PostgreSQL для GORM поверх pgx.
//
// Жёсткое правило CLAUDE.md §4.2 (AQ²-fix #3): pgbouncer в transaction mode
// теряет prepared statements между транзакциями, поэтому подключение обязано
// работать ТОЛЬКО в simple protocol:
//   - pgx: cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol;
//   - GORM: &gorm.Config{PrepareStmt: false} + PreferSimpleProtocol в драйвере.
//
// Никакой другой код проекта не имеет права открывать соединение с БД в обход
// этого пакета.
package db

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	gormpg "gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/interfin/interfin-ai-crm/internal/config"
)

// Open создаёт *gorm.DB по настройкам database из config.yaml.
// config.validate() уже гарантирует prepare_stmt=false и query_exec_mode=simple,
// но этот пакет выставляет режимы сам — конфиг их только подтверждает.
func Open(cfg config.DatabaseConfig) (*gorm.DB, error) {
	pgxCfg, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("db: parse dsn: %w", err)
	}
	pgxCfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol // CLAUDE.md §4.2

	sqlDB := stdlib.OpenDB(*pgxCfg)
	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
		sqlDB.SetMaxIdleConns(cfg.MaxOpenConns)
	}
	sqlDB.SetConnMaxLifetime(30 * time.Minute)

	gormDB, err := gorm.Open(gormpg.New(gormpg.Config{
		Conn:                 sqlDB,
		PreferSimpleProtocol: true, // CLAUDE.md §4.2
	}), &gorm.Config{
		PrepareStmt: false, // CLAUDE.md §4.2
		Logger:      logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("db: gorm open: %w", err)
	}

	slog.Info("db: подключение открыто",
		"query_exec_mode", "simple_protocol",
		"prepare_stmt", false,
		"max_open_conns", cfg.MaxOpenConns)
	return gormDB, nil
}
