// EP-06: финальная ошибка индексации КБ → событие error/kb_index
// (detail — имя файла + причина) + алерт владельцу (фикстура scan.pdf
// из EP-03 — критерий приёмки).
package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/interfin/interfin-ai-crm/internal/kbtext"
	"github.com/interfin/interfin-ai-crm/internal/models"
)

func TestEP06_KBIndexErrorEventAndAlert(t *testing.T) {
	scan, err := os.ReadFile(filepath.Join("..", "kbtext", "testdata", "scan.pdf"))
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	path := filepath.Join(t.TempDir(), "uuid-scan")
	if err := os.WriteFile(path, scan, 0o644); err != nil {
		t.Fatal(err)
	}
	files := newFakeKBFiles(models.EmmaKBFile{
		ID: 7, Filename: "scan.pdf", MimeType: kbtext.MimePDF, FilePath: path,
		FileSize: int64(len(scan)), IndexStatus: models.EmmaKBPending,
	})
	events := &fakeEvents{}
	alerts := &fakeAlerts{}
	h := NewEmmaKBHandlers(EmmaKBDeps{
		Files: files, Indexer: &fakeKBIndexer{}, Pub: &fakePublisher{},
		Events: events, Alerts: alerts, Log: testLogger(),
	})

	if err := h.HandleEmmaKBIndex(context.Background(), kbTask(t, 7)); !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("ждали SkipRetry, получили %v", err)
	}

	errs := events.byType(models.EmmaEventError)
	if len(errs) != 1 || errs[0].ErrorKind == nil || *errs[0].ErrorKind != models.EmmaErrKBIndex {
		t.Fatalf("ждали одно событие kb_index: %+v", errs)
	}
	detail := *errs[0].Detail
	if !strings.Contains(detail, "scan.pdf") {
		t.Errorf("detail без имени файла: %q", detail)
	}
	if !strings.Contains(detail, kbtext.ErrNoTextLayer.Error()) {
		t.Errorf("detail без причины: %q", detail)
	}

	kinds, _ := alerts.snapshot()
	if len(kinds) != 1 || kinds[0] != models.EmmaErrKBIndex {
		t.Errorf("алерты получили %v, ждали [kb_index] (каждая ошибка индексации)", kinds)
	}
}
