// cmd/create-manager — служебный CLI M7: заводит учётку менеджера/админа
// (таблица managers, §5.2). Без него в системе нет ни одной учётки —
// /auth/login некому выдавать токены.
//
// Пароль читается со stdin (не флагом — аргументы видны в ps/истории шелла)
// и сохраняется ТОЛЬКО bcrypt-хешем:
//
//	POSTGRES_DSN=postgres://... go run ./cmd/create-manager \
//	    -email admin@interfin.com -name "Admin" -role admin
//	Password: ********
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "create-manager:", err)
		os.Exit(1)
	}
}

func run() error {
	email := flag.String("email", "", "email менеджера (логин, обязателен)")
	name := flag.String("name", "", "имя (опционально)")
	role := flag.String("role", auth.RoleManager, "роль: manager | admin (§5.2)")
	flag.Parse()

	if *email == "" {
		return fmt.Errorf("флаг -email обязателен")
	}
	if *role != auth.RoleManager && *role != auth.RoleAdmin {
		// system — токены сервисов, а не учётка в managers (§5.2).
		return fmt.Errorf("роль %q недопустима: только manager или admin", *role)
	}
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("не задан POSTGRES_DSN")
	}

	fmt.Fprint(os.Stderr, "Password: ")
	reader := bufio.NewReader(os.Stdin)
	password, err := reader.ReadString('\n')
	if err != nil && password == "" {
		return fmt.Errorf("чтение пароля: %w", err)
	}
	password = strings.TrimRight(password, "\r\n")
	if len(password) < 8 {
		return fmt.Errorf("пароль короче 8 символов")
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}

	gdb, err := db.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 2})
	if err != nil {
		return err
	}
	managers, _ := repo.NewAuth(gdb)

	m := &models.Manager{
		Email:        *email,
		PasswordHash: hash,
		Role:         *role,
		Active:       true, // явно: zero value затёр бы DEFAULT TRUE схемы (грабля M5)
	}
	if *name != "" {
		m.Name = name
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := managers.Create(ctx, m); err != nil {
		return err
	}
	fmt.Printf("создан менеджер id=%d email=%s role=%s\n", m.ID, m.Email, m.Role)
	return nil
}
