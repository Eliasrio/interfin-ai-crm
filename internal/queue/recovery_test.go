package queue_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
)

// fakePendingRepo — PendingLeadRepo в памяти.
type fakePendingRepo struct {
	pending map[int64]int // lead_id → текущий message_count в «БД»
	cleared []int64
}

func (f *fakePendingRepo) ListPendingTask(_ context.Context, limit int) ([]models.Lead, error) {
	var leads []models.Lead
	for id, count := range f.pending {
		leads = append(leads, models.Lead{ID: id, MessageCount: count, PendingTask: true})
		if len(leads) == limit {
			break
		}
	}
	return leads, nil
}

func (f *fakePendingRepo) ClearPendingTask(_ context.Context, id int64, seen int) (bool, error) {
	// Как в SQL: снимаем, только если message_count не изменился.
	if current, ok := f.pending[id]; ok && current == seen {
		delete(f.pending, id)
		f.cleared = append(f.cleared, id)
		return true, nil
	}
	return false, nil
}

// fakeEnqueuer — Enqueuer с программируемыми ошибками.
type fakeEnqueuer struct {
	errs  map[int64]error // lead_id → ошибка enqueue (nil = успех)
	calls []int64
	msgs  []int
}

func (f *fakeEnqueuer) EnqueueInbound(_ context.Context, leadID int64, msgID int) error {
	f.calls = append(f.calls, leadID)
	f.msgs = append(f.msgs, msgID)
	return f.errs[leadID]
}

func newRecovery(repo *fakePendingRepo, enq *fakeEnqueuer) *queue.Recovery {
	return queue.NewRecovery(repo, enq, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// Redis ожил: задача перевыставлена, флаг снят, дедуп-ключ от -message_count.
func TestRecoverySweep_EnqueuesAndClears(t *testing.T) {
	repo := &fakePendingRepo{pending: map[int64]int{7: 3}}
	enq := &fakeEnqueuer{}

	newRecovery(repo, enq).Sweep(context.Background())

	if len(enq.calls) != 1 || enq.calls[0] != 7 {
		t.Fatalf("ждали один enqueue лида 7, получили %v", enq.calls)
	}
	if enq.msgs[0] != -3 {
		t.Errorf("дедуп msgID = %d, ждали -message_count = -3", enq.msgs[0])
	}
	if len(repo.pending) != 0 {
		t.Errorf("pending_task не снят: %v", repo.pending)
	}
}

// ErrDuplicate — прошлый Sweep уже поставил задачу, но упал до снятия флага:
// не ошибка, флаг снимается.
func TestRecoverySweep_DuplicateStillClears(t *testing.T) {
	repo := &fakePendingRepo{pending: map[int64]int{7: 3}}
	enq := &fakeEnqueuer{errs: map[int64]error{7: queue.ErrDuplicate}}

	newRecovery(repo, enq).Sweep(context.Background())

	if len(repo.pending) != 0 {
		t.Errorf("pending_task не снят после ErrDuplicate: %v", repo.pending)
	}
}

// Redis всё ещё лежит: флаг НЕ снимается, сообщение не теряется.
func TestRecoverySweep_RedisStillDownKeepsFlag(t *testing.T) {
	repo := &fakePendingRepo{pending: map[int64]int{7: 3}}
	enq := &fakeEnqueuer{errs: map[int64]error{7: errors.New("redis: connection refused")}}

	newRecovery(repo, enq).Sweep(context.Background())

	if _, ok := repo.pending[7]; !ok {
		t.Fatal("pending_task снят при недоступном Redis — сообщение потеряно")
	}
}

// Гонка §11.2: между enqueue и снятием флага лид прислал новое сообщение
// (message_count вырос, его enqueue снова упал). Guard по message_count
// обязан оставить флаг — новое сообщение доберёт следующий тик.
func TestRecoverySweep_NewMessageDuringSweepKeepsFlag(t *testing.T) {
	repo := &fakePendingRepo{pending: map[int64]int{7: 3}}
	enq := &fakeEnqueuer{}
	rec := queue.NewRecovery(repo, raceEnqueuer{inner: enq, repo: repo, leadID: 7}, 0,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec.Sweep(context.Background())

	if count, ok := repo.pending[7]; !ok || count != 4 {
		t.Fatalf("флаг обязан пережить Sweep с новым сообщением, pending=%v", repo.pending)
	}
}

// raceEnqueuer имитирует вебхук, вклинившийся между enqueue и ClearPendingTask:
// во время enqueue у лида появляется новое сообщение (count+1, флаг снова TRUE).
type raceEnqueuer struct {
	inner  *fakeEnqueuer
	repo   *fakePendingRepo
	leadID int64
}

func (r raceEnqueuer) EnqueueInbound(ctx context.Context, leadID int64, msgID int) error {
	err := r.inner.EnqueueInbound(ctx, leadID, msgID)
	r.repo.pending[r.leadID]++ // новое inbound: message_count вырос, pending_task снова TRUE
	return err
}
