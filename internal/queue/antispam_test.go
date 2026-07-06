package queue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/config"
)

// scheduledAntiSpam — задачи типа taskType в scheduled; ETA — по dedup-ключу.
func scheduledAntiSpam(t *testing.T, addr, taskType string) int {
	t.Helper()
	insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: addr})
	defer insp.Close()
	tasks, err := insp.ListScheduledTasks(antiSpamQueue)
	if err != nil {
		if errors.Is(err, asynq.ErrQueueNotFound) {
			return 0
		}
		t.Fatalf("list scheduled: %v", err)
	}
	n := 0
	for _, task := range tasks {
		if task.Type == taskType {
			n++
		}
	}
	return n
}

func antiSpamETA(t *testing.T, addr, taskType string, leadID int64) time.Time {
	t.Helper()
	insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: addr})
	defer insp.Close()
	info, err := insp.GetTaskInfo(antiSpamQueue, AntiSpamDedupKey(taskType, leadID))
	if err != nil {
		t.Fatalf("задача %s лида %d не найдена: %v", taskType, leadID, err)
	}
	return info.NextProcessAt
}

// TestAntiSpamManager_ScheduleCancel — §3.5/AQ²-8 против реального Redis:
// взвод пары followup(24ч)+escalate(48ч), идемпотентность повторного
// взвода (fresh=false), снятие обеих при переходе стадии.
func TestAntiSpamManager_ScheduleCancel(t *testing.T) {
	addr := testRedis(t) // skip без REDIS_TEST_ADDR
	insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: addr})
	if _, err := insp.DeleteAllScheduledTasks(antiSpamQueue); err != nil &&
		!errors.Is(err, asynq.ErrQueueNotFound) {
		t.Logf("очистка scheduled: %v", err)
	}
	insp.Close()

	const leadID = int64(77)
	m := NewAntiSpamManager(config.RedisConfig{Addr: addr})
	defer m.Close()
	ctx := context.Background()

	// Взвод: обе задачи в scheduled с корректными ETA (24ч / 48ч).
	fresh, err := m.Schedule(ctx, leadID, 2, 24*time.Hour, 48*time.Hour)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if !fresh {
		t.Error("первый взвод обязан вернуть fresh=true (по нему публикуется alert)")
	}
	if f, e := scheduledAntiSpam(t, addr, TypeAntiSpamFollowup), scheduledAntiSpam(t, addr, TypeAntiSpamEscalate); f != 1 || e != 1 {
		t.Fatalf("followup=%d escalate=%d, ожидали 1/1", f, e)
	}
	now := time.Now()
	if eta := antiSpamETA(t, addr, TypeAntiSpamFollowup, leadID); eta.Sub(now.Add(24*time.Hour)).Abs() > time.Minute {
		t.Errorf("followup ETA %v, ожидали ~+24ч (§3.5)", eta)
	}
	if eta := antiSpamETA(t, addr, TypeAntiSpamEscalate, leadID); eta.Sub(now.Add(48*time.Hour)).Abs() > time.Minute {
		t.Errorf("escalate ETA %v, ожидали ~+48ч (AQ²-8)", eta)
	}

	// Повторный взвод (ретрай process:inbound / конкурентное 25-е сообщение):
	// задачи не дублируются, fresh=false — alert не публикуется повторно.
	fresh, err = m.Schedule(ctx, leadID, 2, 24*time.Hour, 48*time.Hour)
	if err != nil {
		t.Fatalf("повторный schedule: %v", err)
	}
	if fresh {
		t.Error("повторный взвод вернул fresh=true — задвоился бы antispam_alert")
	}
	if f, e := scheduledAntiSpam(t, addr, TypeAntiSpamFollowup), scheduledAntiSpam(t, addr, TypeAntiSpamEscalate); f != 1 || e != 1 {
		t.Errorf("после повторного взвода followup=%d escalate=%d, ожидали 1/1 (§4.5)", f, e)
	}

	// Переход стадии → Cancel снимает обе (§3.5: сброс при переходе).
	if err := m.Cancel(ctx, leadID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if f, e := scheduledAntiSpam(t, addr, TypeAntiSpamFollowup), scheduledAntiSpam(t, addr, TypeAntiSpamEscalate); f != 0 || e != 0 {
		t.Errorf("после cancel followup=%d escalate=%d, ожидали 0/0", f, e)
	}

	// Повторный Cancel — no-op (задач уже нет), не ошибка.
	if err := m.Cancel(ctx, leadID); err != nil {
		t.Errorf("повторный cancel: %v", err)
	}

	// После Cancel лимит может сработать в следующей стадии — перевзвод
	// возможен (unique-замок не мешает, CLAUDE.md-грабля M3 не повторена).
	fresh, err = m.Schedule(ctx, leadID, 3, 24*time.Hour, 48*time.Hour)
	if err != nil || !fresh {
		t.Fatalf("перевзвод после cancel: fresh=%v err=%v", fresh, err)
	}
	if err := m.Cancel(ctx, leadID); err != nil {
		t.Fatalf("финальная зачистка: %v", err)
	}
}
