// Интеграционные тесты EP-03.
//
// TestEmmaKB_ReuploadReplacesChunks — критерий приёмки «повторная загрузка
// того же filename: id тот же, чанки заменены, осиротевших source нет»:
// реальный PostgreSQL (POSTGRES_TEST_DSN) + реальные rag.Indexer/чанкер +
// фейковый Embedder (без Voyage).
//
// TestEmmaKB_Voyage429_RetriesThenIndexed / TestEmmaKB_ScanPDF_NoRetry —
// дисциплина ретраев на настоящем Asynq (REDIS_TEST_ADDR): 429 ретраится
// до indexed, скан падает в error БЕЗ ретрая.
package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/config"
	"github.com/interfin/interfin-ai-crm/internal/db"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/kbtext"
	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/queue"
	"github.com/interfin/interfin-ai-crm/internal/rag"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// fakeEmbedder — детерминированные векторы 1024 (контракт rag.Embedder);
// Voyage в тесте не участвует.
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, texts []string, _ string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, 1024)
		v[0] = 1
		out[i] = v
	}
	return out, nil
}

func TestEmmaKB_ReuploadReplacesChunks(t *testing.T) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN не задан — интеграционный тест пропущен")
	}
	gdb, err := db.Open(config.DatabaseConfig{DSN: dsn, MaxOpenConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec(`TRUNCATE emma_kb_files, knowledge_chunks RESTART IDENTITY`).Error; err != nil {
		t.Fatalf("truncate: %v (миграции накатаны?)", err)
	}

	files := repo.NewEmmaKB(gdb)
	knowledge, _, _ := repo.NewRAG(gdb)
	pub := &fakePublisher{}
	h := NewEmmaKBHandlers(EmmaKBDeps{
		Files:   files,
		Indexer: rag.NewIndexer(fakeEmbedder{}, knowledge, testLogger()),
		Pub:     pub,
		Log:     testLogger(),
	})
	ctx := context.Background()
	dir := t.TempDir()

	countChunks := func(source string) int64 {
		var n int64
		if err := gdb.Model(&models.KnowledgeChunk{}).
			Where("source = ?", source).Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}

	// v1: ~3 чанка (чанкер режет по 1200 символов).
	upload := func(content string) *models.EmmaKBFile {
		t.Helper()
		path := filepath.Join(dir, "v-"+time.Now().Format("150405.000000000"))
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		f := &models.EmmaKBFile{
			Filename: "prices.txt", MimeType: kbtext.MimeTXT,
			FilePath: path, FileSize: int64(len(content)),
		}
		if _, err := files.UpsertByFilename(ctx, f); err != nil {
			t.Fatal(err)
		}
		if err := h.HandleEmmaKBIndex(ctx, kbTask(t, f.ID)); err != nil {
			t.Fatalf("index: %v", err)
		}
		return f
	}

	f1 := upload(strings.Repeat("Прайс версии один. ", 200)) // ~3800 байт → >1 чанка
	source := models.EmmaKBSource(f1.ID)

	got1, err := files.GetByID(ctx, f1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got1.IndexStatus != models.EmmaKBIndexed || got1.ChunksCount < 2 {
		t.Fatalf("v1: %+v", got1)
	}
	if n := countChunks(source); n != int64(got1.ChunksCount) {
		t.Fatalf("v1: чанков в БД %d, в строке %d", n, got1.ChunksCount)
	}

	// v2 короче — чанков меньше; строка ТА ЖЕ, чанки заменены атомарно.
	f2 := upload("Прайс версии два: коротко.")
	if f2.ID != f1.ID {
		t.Fatalf("id сменился: %d → %d", f1.ID, f2.ID)
	}
	got2, _ := files.GetByID(ctx, f2.ID)
	if got2.IndexStatus != models.EmmaKBIndexed || got2.ChunksCount != 1 {
		t.Fatalf("v2: %+v", got2)
	}
	if n := countChunks(source); n != 1 {
		t.Fatalf("v2: чанков source %d, ждали 1 (старые не вычищены)", n)
	}
	// Осиротевших panel:* source нет — все чанки панели висят на живой строке.
	var orphans int64
	if err := gdb.Raw(`
		SELECT count(*) FROM knowledge_chunks
		WHERE source LIKE 'panel:%'
		  AND source <> ?`, source).Scan(&orphans).Error; err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Fatalf("осиротевших чанков panel:*: %d", orphans)
	}

	// Финал каждого прогона — WS-событие; оба indexed.
	if n := pub.countByType(events.TypeEmmaKBStatus); n != 2 {
		t.Fatalf("событий emma_kb_status: %d, ждали 2", n)
	}

	// Критерий приёмки: DELETE — чанков source нет, строки нет.
	if _, err := files.Delete(ctx, f2.ID); err != nil {
		t.Fatal(err)
	}
	if n := countChunks(source); n != 0 {
		t.Fatalf("после DELETE осталось %d чанков", n)
	}
}

// Критерий приёмки: фейк Voyage, дважды отвечающий 429, → задача ретраится
// и завершается indexed; WS-событие ровно одно (только финал).
func TestEmmaKB_Voyage429_RetriesThenIndexed(t *testing.T) {
	redisCfg := testRedis(t)
	cleanQueues(t, redisCfg.Addr)

	path := filepath.Join(t.TempDir(), "uuid-1")
	if err := os.WriteFile(path, []byte("Секретный код 998877."), 0o644); err != nil {
		t.Fatal(err)
	}
	files := newFakeKBFiles(models.EmmaKBFile{
		ID: 7, Filename: "code.txt", MimeType: kbtext.MimeTXT,
		FilePath: path, FileSize: 22, IndexStatus: models.EmmaKBPending,
	})
	ix := &fakeKBIndexer{failures: 2, chunks: 1} // 429, 429, успех
	pub := &fakePublisher{}

	client := queue.NewClient(redisCfg)
	defer client.Close()
	if err := client.EnqueueKBIndex(context.Background(), 7, time.Now().Unix()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Боевой backoff растянул бы тест — ужимаем (образец Claude-down теста).
	leads := newFakeLeads(testLead)
	msgs := &fakeMsgs{}
	snd := &fakeSender{}
	srv := New(redisCfg,
		newTestProcessor(t, leads, msgs, &fakeAI{reply: "x"}, snd),
		newTestSummarizer(leads, msgs, &fakeSummaries{}, &fakeAI{reply: "x"}),
		snd, 0, testLogger(),
		WithRetryDelayFunc(func(int, error, *asynq.Task) time.Duration {
			return 50 * time.Millisecond
		}),
		func(c *asynq.Config) { c.DelayedTaskCheckInterval = 100 * time.Millisecond })
	srv.RegisterEmmaKB(NewEmmaKBHandlers(EmmaKBDeps{
		Files: files, Indexer: ix, Pub: pub, Log: testLogger(),
	}))
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Shutdown()

	waitFor(t, 10*time.Second, func() bool {
		return files.row(t, 7).IndexStatus == models.EmmaKBIndexed
	}, "статус indexed после двух 429")

	if got := ix.callCount(); got != 3 {
		t.Errorf("индексатор вызван %d раз, ждали 3 (429, 429, успех)", got)
	}
	if n := pub.countByType(events.TypeEmmaKBStatus); n != 1 {
		t.Errorf("событий emma_kb_status %d, ждали 1 (только финал)", n)
	}
	if row := files.row(t, 7); row.ChunksCount != 1 || row.IndexError != nil {
		t.Errorf("строка: %+v", row)
	}
}

// Критерий приёмки: PDF-скан на настоящем Asynq — задача НЕ ретраится
// (SkipRetry → сразу архив), обработчик взял файл ровно один раз.
func TestEmmaKB_ScanPDF_NoRetry(t *testing.T) {
	redisCfg := testRedis(t)
	cleanQueues(t, redisCfg.Addr)

	scan, err := os.ReadFile(filepath.Join("..", "kbtext", "testdata", "scan.pdf"))
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	path := filepath.Join(t.TempDir(), "uuid-scan")
	if err := os.WriteFile(path, scan, 0o644); err != nil {
		t.Fatal(err)
	}
	files := newFakeKBFiles(models.EmmaKBFile{
		ID: 8, Filename: "scan.pdf", MimeType: kbtext.MimePDF,
		FilePath: path, FileSize: int64(len(scan)), IndexStatus: models.EmmaKBPending,
	})
	ix := &fakeKBIndexer{}
	pub := &fakePublisher{}

	client := queue.NewClient(redisCfg)
	defer client.Close()
	if err := client.EnqueueKBIndex(context.Background(), 8, time.Now().Unix()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	leads := newFakeLeads(testLead)
	msgs := &fakeMsgs{}
	snd := &fakeSender{}
	srv := New(redisCfg,
		newTestProcessor(t, leads, msgs, &fakeAI{reply: "x"}, snd),
		newTestSummarizer(leads, msgs, &fakeSummaries{}, &fakeAI{reply: "x"}),
		snd, 0, testLogger(),
		WithRetryDelayFunc(func(int, error, *asynq.Task) time.Duration {
			return 50 * time.Millisecond
		}),
		func(c *asynq.Config) { c.DelayedTaskCheckInterval = 100 * time.Millisecond })
	srv.RegisterEmmaKB(NewEmmaKBHandlers(EmmaKBDeps{
		Files: files, Indexer: ix, Pub: pub, Log: testLogger(),
	}))
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Shutdown()

	waitFor(t, 5*time.Second, func() bool {
		return files.row(t, 8).IndexStatus == models.EmmaKBError
	}, "статус error для скана")

	// SkipRetry → задача сразу в архиве, без ретраев.
	insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: redisCfg.Addr})
	defer insp.Close()
	waitFor(t, 5*time.Second, func() bool {
		tasks, err := insp.ListArchivedTasks("default")
		return err == nil && len(tasks) == 1
	}, "задача в архиве (SkipRetry)")

	// Даём воркеру шанс на лишний ретрай, если бы он был.
	time.Sleep(300 * time.Millisecond)
	if got := files.getCount(); got != 1 {
		t.Errorf("обработчик брал файл %d раз, ждали 1 (без ретраев)", got)
	}
	row := files.row(t, 8)
	if row.IndexError == nil || !strings.Contains(*row.IndexError, "OCR не поддерживается") {
		t.Errorf("текст ошибки: %v", row.IndexError)
	}
}
