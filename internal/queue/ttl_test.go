package queue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// fakeLeadRepo — минимальный LeadRepo для TTL-тестов: хранит лидов в памяти,
// из UpdateFields понимает только ttl_task_id (больше TTLManager не трогает).
type fakeLeadRepo struct {
	mu    sync.Mutex
	leads map[int64]*models.Lead
}

func newFakeLeadRepo(leads ...*models.Lead) *fakeLeadRepo {
	f := &fakeLeadRepo{leads: map[int64]*models.Lead{}}
	for _, l := range leads {
		f.leads[l.ID] = l
	}
	return f
}

func (f *fakeLeadRepo) Create(_ context.Context, lead *models.Lead) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leads[lead.ID] = lead
	return nil
}

func (f *fakeLeadRepo) GetByID(_ context.Context, id int64) (*models.Lead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lead, ok := f.leads[id]
	if !ok {
		return nil, repo.ErrNotFound
	}
	cp := *lead
	return &cp, nil
}

func (f *fakeLeadRepo) GetByTelegramUserID(context.Context, int64) (*models.Lead, error) {
	return nil, repo.ErrNotFound
}

func (f *fakeLeadRepo) List(context.Context, repo.ListLeadsParams) ([]models.Lead, int64, error) {
	panic("TTL-менеджер не листает лидов (метод M8)")
}

func (f *fakeLeadRepo) Save(_ context.Context, lead *models.Lead) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leads[lead.ID] = lead
	return nil
}

func (f *fakeLeadRepo) UpdateFields(_ context.Context, id int64, fields map[string]interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	lead, ok := f.leads[id]
	if !ok {
		return repo.ErrNotFound
	}
	if v, ok := fields["ttl_task_id"]; ok {
		if v == nil {
			lead.TTLTaskID = nil
		} else {
			s := v.(string)
			lead.TTLTaskID = &s
		}
	}
	return nil
}

func (f *fakeLeadRepo) TransitionStage(_ context.Context, id int64, from, to int16) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lead, ok := f.leads[id]
	if !ok || lead.StageID != from {
		return false, nil
	}
	lead.StageID = to
	lead.AntiSpamCount = 0
	lead.LastActivityAt = time.Now() // точка отсчёта TTL (CLAUDE.md §4.7)
	return true, nil
}

func (f *fakeLeadRepo) ttlTaskID(t *testing.T, id int64) *string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	lead, ok := f.leads[id]
	if !ok {
		t.Fatalf("лид %d потерян фейком", id)
	}
	return lead.TTLTaskID
}

func scheduledTTLCount(t *testing.T, addr string) int {
	t.Helper()
	insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: addr})
	defer insp.Close()
	tasks, err := insp.ListScheduledTasks(ttlQueue)
	if err != nil {
		if errors.Is(err, asynq.ErrQueueNotFound) {
			return 0
		}
		t.Fatalf("list scheduled: %v", err)
	}
	n := 0
	for _, task := range tasks {
		if task.Type == TypeTTLExpire {
			n++
		}
	}
	return n
}

// TestTTLManager_ScheduleRescheduleCancel — контракт §6.4 для M5:
// schedule сохраняет info.ID в leads.ttl_task_id, повторный schedule заменяет
// задачу (reset TTL, CLAUDE.md §4.7), cancel снимает и чистит поле.
func TestTTLManager_ScheduleRescheduleCancel(t *testing.T) {
	addr := testRedis(t) // из queue_test.go: skip без REDIS_TEST_ADDR
	insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: addr})
	if _, err := insp.DeleteAllScheduledTasks(ttlQueue); err != nil &&
		!errors.Is(err, asynq.ErrQueueNotFound) {
		t.Logf("очистка scheduled: %v", err)
	}
	insp.Close()

	const leadID = int64(31)
	leads := newFakeLeadRepo(&models.Lead{ID: leadID, TelegramUserID: 111})
	m := NewTTLManager(config.RedisConfig{Addr: addr}, leads)
	defer m.Close()

	ctx := context.Background()

	// Schedule: задача в scheduled, её ID — в leads.ttl_task_id (§6.4).
	// ID детерминированный (CLAUDE.md §4.5): максимум одна задача на лида.
	if err := m.Schedule(ctx, leadID, 48*time.Hour); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	firstID := leads.ttlTaskID(t, leadID)
	if firstID == nil || *firstID != TTLDedupKey(leadID) {
		t.Fatalf("ttl_task_id = %v, ожидали TTLDedupKey(%d)", firstID, leadID)
	}
	if n := scheduledTTLCount(t, addr); n != 1 {
		t.Fatalf("scheduled ttl-задач: %d, ожидали 1", n)
	}

	// Reset TTL = повторный Schedule (CLAUDE.md §4.7: DeleteTask + enqueue):
	// задача одна, но время срабатывания пересчитано от нового delay.
	if err := m.Schedule(ctx, leadID, 24*time.Hour); err != nil {
		t.Fatalf("reschedule: %v", err)
	}
	if n := scheduledTTLCount(t, addr); n != 1 {
		t.Errorf("после reschedule scheduled ttl-задач: %d, ожидали 1 (старая не снята?)", n)
	}
	insp2 := asynq.NewInspector(asynq.RedisClientOpt{Addr: addr})
	info, err := insp2.GetTaskInfo(ttlQueue, TTLDedupKey(leadID))
	insp2.Close()
	if err != nil {
		t.Fatalf("задача после reschedule не найдена: %v", err)
	}
	wantETA := time.Now().Add(24 * time.Hour)
	if diff := info.NextProcessAt.Sub(wantETA); diff < -time.Minute || diff > time.Minute {
		t.Errorf("после reschedule задача сработает в %v, ожидали ~%v (delay не сброшен)",
			info.NextProcessAt, wantETA)
	}

	// Cancel: задачи нет, поле чисто.
	if err := m.Cancel(ctx, leadID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if leads.ttlTaskID(t, leadID) != nil {
		t.Error("ttl_task_id не очищен после Cancel")
	}
	if n := scheduledTTLCount(t, addr); n != 0 {
		t.Errorf("после cancel scheduled ttl-задач: %d, ожидали 0", n)
	}

	// Повторный Cancel — no-op, не ошибка (задачи уже нет).
	if err := m.Cancel(ctx, leadID); err != nil {
		t.Errorf("повторный cancel: %v", err)
	}
}
