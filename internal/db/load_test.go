package db_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// Мини-версия AQ²-3 (полная — в M11 через pgbouncer): конкурентная нагрузка
// через repo-слой не должна дать ни одной ошибки "prepared statement", а на
// соединениях пула не должно быть server-side prepared statements вообще —
// доказательство, что pgx реально работает в SimpleProtocol (CLAUDE.md §4.2).
func TestSimpleProtocolUnderLoad(t *testing.T) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN не задан — нагрузочный тест пропущен")
	}

	gdb, err := db.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec(`TRUNCATE leads, messages, payment_events RESTART IDENTITY CASCADE`).Error; err != nil {
		t.Fatalf("truncate: %v (схема накатана? go run ./cmd/migrate up)", err)
	}
	leads, msgs, _ := repo.New(gdb)
	ctx := context.Background()

	const workers = 20
	const opsPerWorker = 100

	var wg sync.WaitGroup
	errCh := make(chan error, workers*opsPerWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			lead := &models.Lead{TelegramUserID: int64(1_000_000 + w)}
			if err := leads.Create(ctx, lead); err != nil {
				errCh <- err
				return
			}
			for i := 0; i < opsPerWorker; i++ {
				var err error
				switch i % 4 {
				case 0:
					err = msgs.CreateInbound(ctx, &models.Message{LeadID: lead.ID, Content: "in"})
				case 1:
					err = msgs.CreateOutbound(ctx, &models.Message{LeadID: lead.ID, Content: "out"})
				case 2:
					_, err = leads.GetByTelegramUserID(ctx, lead.TelegramUserID)
				case 3:
					_, err = msgs.ListByLead(ctx, lead.ID, 10)
				}
				if err != nil {
					errCh <- fmt.Errorf("worker %d op %d: %w", w, i, err)
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)

	total, prepared := 0, 0
	for err := range errCh {
		total++
		if strings.Contains(err.Error(), "prepared statement") {
			prepared++
		}
		if total <= 5 {
			t.Errorf("ошибка под нагрузкой: %v", err)
		}
	}
	if total > 0 {
		t.Fatalf("под нагрузкой %d ошибок, из них про prepared statement: %d (ждали 0/0)", total, prepared)
	}

	// Опрашиваем соединения пула: SimpleProtocol не создаёт server-side
	// prepared statements, pg_prepared_statements обязан быть пуст.
	for i := 0; i < workers; i++ {
		var n int
		if err := gdb.Raw(`SELECT count(*) FROM pg_prepared_statements`).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("на соединении найдено %d prepared statements — SimpleProtocol не активен", n)
		}
	}

	// Контроль целостности счётчиков после гонки: у каждого лида ровно
	// opsPerWorker/4 inbound (CLAUDE.md §4.3, конкурентные инкременты не теряются).
	var badLeads int
	err = gdb.Raw(`SELECT count(*) FROM leads WHERE message_count <> ?`, opsPerWorker/4).Scan(&badLeads).Error
	if err != nil {
		t.Fatal(err)
	}
	if badLeads != 0 {
		t.Fatalf("%d лидов с неверным message_count после конкурентной нагрузки", badLeads)
	}
	t.Logf("нагрузка: %d операций, 0 ошибок, prepared statements: 0", workers*opsPerWorker)
}
