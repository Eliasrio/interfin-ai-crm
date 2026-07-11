// Юнит-тесты HandleEmmaKBIndex (EP-03): дисциплина ретраев — ошибка
// извлечения (скан, битый файл, нет оригинала) перманентна (SkipRetry,
// status error, WS-событие), ошибка индексатора (Voyage/БД) ретраится
// (статус остаётся pending, события нет).
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/kbtext"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// fakeKBFiles — in-memory repo.EmmaKBRepo для воркера.
type fakeKBFiles struct {
	mu    sync.Mutex
	files map[int64]*models.EmmaKBFile
	gets  int // сколько раз задача бралась за файл (счётчик попыток)
}

func newFakeKBFiles(files ...models.EmmaKBFile) *fakeKBFiles {
	f := &fakeKBFiles{files: map[int64]*models.EmmaKBFile{}}
	for i := range files {
		cp := files[i]
		f.files[cp.ID] = &cp
	}
	return f
}

func (f *fakeKBFiles) List(context.Context) ([]models.EmmaKBFile, error) { return nil, nil }

func (f *fakeKBFiles) GetByID(_ context.Context, id int64) (*models.EmmaKBFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	row, ok := f.files[id]
	if !ok {
		return nil, repo.ErrNotFound
	}
	cp := *row
	return &cp, nil
}

func (f *fakeKBFiles) UpsertByFilename(context.Context, *models.EmmaKBFile) (string, error) {
	return "", errors.New("не используется воркером")
}

func (f *fakeKBFiles) SetStatus(_ context.Context, id int64, status string, chunks int, errText *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.files[id]
	if !ok {
		return repo.ErrNotFound
	}
	row.IndexStatus, row.ChunksCount, row.IndexError = status, chunks, errText
	return nil
}

func (f *fakeKBFiles) Delete(context.Context, int64) (*models.EmmaKBFile, error) {
	return nil, errors.New("не используется воркером")
}

func (f *fakeKBFiles) row(t *testing.T, id int64) models.EmmaKBFile {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.files[id]
	if !ok {
		t.Fatalf("файла %d нет", id)
	}
	return *row
}

func (f *fakeKBFiles) getCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets
}

// fakeKBIndexer — KBIndexer: failures первых вызовов падают (429 Voyage),
// дальше успех с chunks.
type fakeKBIndexer struct {
	mu       sync.Mutex
	failures int
	chunks   int
	calls    int
	sources  []string
}

func (ix *fakeKBIndexer) IndexDocument(_ context.Context, source, _ string) (int, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.calls++
	ix.sources = append(ix.sources, source)
	if ix.calls <= ix.failures {
		return 0, errors.New("voyage: 429 Too Many Requests")
	}
	return ix.chunks, nil
}

func (ix *fakeKBIndexer) callCount() int {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.calls
}

// kbTask — задача emma:kb:index с payload {file_id}.
func kbTask(t *testing.T, fileID int64) *asynq.Task {
	t.Helper()
	payload, err := json.Marshal(queue.EmmaKBIndexPayload{FileID: fileID})
	if err != nil {
		t.Fatal(err)
	}
	return asynq.NewTask(queue.TypeEmmaKBIndex, payload)
}

// kbRig — обработчик + фейки; файл кладётся во временный каталог.
func kbRig(t *testing.T, mime string, content []byte, ix *fakeKBIndexer) (*EmmaKBHandlers, *fakeKBFiles, *fakePublisher) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "uuid-1")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	files := newFakeKBFiles(models.EmmaKBFile{
		ID: 7, Filename: "doc", MimeType: mime, FilePath: path,
		FileSize: int64(len(content)), IndexStatus: models.EmmaKBPending,
	})
	pub := &fakePublisher{}
	h := NewEmmaKBHandlers(EmmaKBDeps{Files: files, Indexer: ix, Pub: pub, Log: testLogger()})
	return h, files, pub
}

// lastKBEvent — последнее событие emma_kb_status фейкового publisher'а.
func lastKBEvent(t *testing.T, pub *fakePublisher) events.Event {
	t.Helper()
	pub.mu.Lock()
	defer pub.mu.Unlock()
	for i := len(pub.events) - 1; i >= 0; i-- {
		if pub.events[i].Type == events.TypeEmmaKBStatus {
			return pub.events[i]
		}
	}
	t.Fatal("событие emma_kb_status не опубликовано")
	return events.Event{}
}

func TestEmmaKBIndex_TxtIndexed(t *testing.T) {
	ix := &fakeKBIndexer{chunks: 3}
	h, files, pub := kbRig(t, kbtext.MimeTXT, []byte("Секретный код 998877."), ix)

	if err := h.HandleEmmaKBIndex(context.Background(), kbTask(t, 7)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	row := files.row(t, 7)
	if row.IndexStatus != models.EmmaKBIndexed || row.ChunksCount != 3 || row.IndexError != nil {
		t.Fatalf("строка: %+v", row)
	}
	// source — контракт panel:<file_id> (навсегда).
	if ix.sources[0] != "panel:7" {
		t.Fatalf("source = %q", ix.sources[0])
	}
	ev := lastKBEvent(t, pub)
	if ev.FileID != 7 || ev.KBStatus != models.EmmaKBIndexed || ev.Chunks != 3 {
		t.Fatalf("событие: %+v", ev)
	}
}

// Критерий приёмки: PDF-скан → status error с понятным текстом, задача
// НЕ ретраится (SkipRetry), индексатор не вызывается.
func TestEmmaKBIndex_ScanPDF_ErrorNoRetry(t *testing.T) {
	scan, err := os.ReadFile(filepath.Join("..", "kbtext", "testdata", "scan.pdf"))
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	ix := &fakeKBIndexer{chunks: 1}
	h, files, pub := kbRig(t, kbtext.MimePDF, scan, ix)

	err = h.HandleEmmaKBIndex(context.Background(), kbTask(t, 7))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("ждали SkipRetry, получили %v", err)
	}
	row := files.row(t, 7)
	if row.IndexStatus != models.EmmaKBError || row.IndexError == nil ||
		*row.IndexError != kbtext.ErrNoTextLayer.Error() {
		t.Fatalf("строка: %+v, err=%v", row, row.IndexError)
	}
	if ix.callCount() != 0 {
		t.Error("индексатор вызван для скана")
	}
	ev := lastKBEvent(t, pub)
	if ev.KBStatus != models.EmmaKBError || ev.KBError == "" {
		t.Fatalf("событие: %+v", ev)
	}
}

// Критерий приёмки: reindex после error переводит в indexed — скан дал
// error, оригинал «починили» (контент с текстовым слоем), повторная задача
// (контур reindex: status pending + новый enqueue) завершается indexed.
func TestEmmaKBIndex_ReindexAfterErrorIndexed(t *testing.T) {
	scan, err := os.ReadFile(filepath.Join("..", "kbtext", "testdata", "scan.pdf"))
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	ix := &fakeKBIndexer{chunks: 2}
	h, files, pub := kbRig(t, kbtext.MimePDF, scan, ix)

	// Первый прогон: скан → error без ретрая.
	if err := h.HandleEmmaKBIndex(context.Background(), kbTask(t, 7)); !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("скан: ждали SkipRetry, получили %v", err)
	}
	if row := files.row(t, 7); row.IndexStatus != models.EmmaKBError {
		t.Fatalf("после скана: %+v", row)
	}

	// «Починка фикстуры»: на месте оригинала — PDF с текстовым слоем.
	fixed, err := os.ReadFile(filepath.Join("..", "kbtext", "testdata", "text.pdf"))
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	if err := os.WriteFile(files.row(t, 7).FilePath, fixed, 0o644); err != nil {
		t.Fatal(err)
	}
	// Ручка reindex делает status pending + enqueue — эмулируем и гоняем задачу.
	if err := files.SetStatus(context.Background(), 7, models.EmmaKBPending, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.HandleEmmaKBIndex(context.Background(), kbTask(t, 7)); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	row := files.row(t, 7)
	if row.IndexStatus != models.EmmaKBIndexed || row.ChunksCount != 2 || row.IndexError != nil {
		t.Fatalf("после reindex: %+v", row)
	}
	// Финалов два: error (скан) + indexed (после починки).
	if n := pub.countByType(events.TypeEmmaKBStatus); n != 2 {
		t.Fatalf("событий: %d, ждали 2", n)
	}
}

// PDF с текстовым слоем — indexed (фикстура text.pdf).
func TestEmmaKBIndex_TextPDFIndexed(t *testing.T) {
	pdf, err := os.ReadFile(filepath.Join("..", "kbtext", "testdata", "text.pdf"))
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	ix := &fakeKBIndexer{chunks: 1}
	h, files, _ := kbRig(t, kbtext.MimePDF, pdf, ix)

	if err := h.HandleEmmaKBIndex(context.Background(), kbTask(t, 7)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if row := files.row(t, 7); row.IndexStatus != models.EmmaKBIndexed {
		t.Fatalf("строка: %+v", row)
	}
}

// Оригинал пропал с диска — перманентная ошибка (повторная загрузка ставит
// новую задачу, ретрай этой бессмыслен).
func TestEmmaKBIndex_MissingOriginal_ErrorNoRetry(t *testing.T) {
	ix := &fakeKBIndexer{}
	h, files, _ := kbRig(t, kbtext.MimeTXT, []byte("x"), ix)
	row := files.row(t, 7)
	if err := os.Remove(row.FilePath); err != nil {
		t.Fatal(err)
	}

	err := h.HandleEmmaKBIndex(context.Background(), kbTask(t, 7))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("ждали SkipRetry, получили %v", err)
	}
	if row := files.row(t, 7); row.IndexStatus != models.EmmaKBError {
		t.Fatalf("строка: %+v", row)
	}
}

// Ошибка Voyage/БД — ретрай: err БЕЗ SkipRetry, статус остаётся pending,
// событие не публикуется (финал ещё не наступил).
func TestEmmaKBIndex_IndexerError_Retries(t *testing.T) {
	ix := &fakeKBIndexer{failures: 100}
	h, files, pub := kbRig(t, kbtext.MimeTXT, []byte("текст"), ix)

	err := h.HandleEmmaKBIndex(context.Background(), kbTask(t, 7))
	if err == nil || errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("ждали ретраебельную ошибку, получили %v", err)
	}
	if row := files.row(t, 7); row.IndexStatus != models.EmmaKBPending {
		t.Fatalf("статус сменился до финала: %+v", row)
	}
	if n := pub.countByType(events.TypeEmmaKBStatus); n != 0 {
		t.Fatalf("событий до финала: %d", n)
	}
}

// Файл удалили, пока задача стояла в очереди, — не ошибка.
func TestEmmaKBIndex_RowGone_NoError(t *testing.T) {
	h := NewEmmaKBHandlers(EmmaKBDeps{
		Files: newFakeKBFiles(), Indexer: &fakeKBIndexer{},
		Pub: &fakePublisher{}, Log: testLogger(),
	})
	if err := h.HandleEmmaKBIndex(context.Background(), kbTask(t, 404)); err != nil {
		t.Fatalf("ждали nil, получили %v", err)
	}
}

// Битый payload — SkipRetry (образец process:inbound).
func TestEmmaKBIndex_BadPayload_SkipRetry(t *testing.T) {
	h := NewEmmaKBHandlers(EmmaKBDeps{
		Files: newFakeKBFiles(), Indexer: &fakeKBIndexer{},
		Pub: &fakePublisher{}, Log: testLogger(),
	})
	err := h.HandleEmmaKBIndex(context.Background(),
		asynq.NewTask(queue.TypeEmmaKBIndex, []byte("не json")))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("ждали SkipRetry, получили %v", err)
	}
}
