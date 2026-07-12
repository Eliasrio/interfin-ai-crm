// Contract-тесты EP-04 (маркер-протокол файлов): Send получает текст БЕЗ
// маркера, в messages — чистый текст и след [файл: …], SendDocument/SendPhoto
// по mime, события file_sent / error(file_not_found, telegram_api),
// {{handoff}} — только вырезается (задел EP-05), секция файлов в system-блоке.
package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/models"
	"github.com/interfin/interfin-ai-crm/internal/repo"
)

// --- фейки EP-04 ---

// fakeSendFiles — in-memory repo.EmmaSendFilesRepo (процессору нужны
// GetByID и ListActive; остальное — для полноты интерфейса).
type fakeSendFiles struct {
	mu    sync.Mutex
	files map[int64]models.EmmaSendFile
	err   error // ошибка каждого вызова — сценарий «БД мигнула»

	listActiveCalls int
}

func newFakeSendFiles(files ...models.EmmaSendFile) *fakeSendFiles {
	m := map[int64]models.EmmaSendFile{}
	for _, f := range files {
		m[f.ID] = f
	}
	return &fakeSendFiles{files: m}
}

func (f *fakeSendFiles) List(context.Context) ([]models.EmmaSendFile, error) {
	return f.ListActive(context.Background())
}

func (f *fakeSendFiles) GetByID(_ context.Context, id int64) (*models.EmmaSendFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	file, ok := f.files[id]
	if !ok {
		return nil, repo.ErrNotFound
	}
	return &file, nil
}

func (f *fakeSendFiles) Create(_ context.Context, file *models.EmmaSendFile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[file.ID] = *file
	return nil
}

func (f *fakeSendFiles) Update(context.Context, int64, repo.EmmaSendFileUpdate) (*models.EmmaSendFile, error) {
	return nil, repo.ErrNotFound
}

func (f *fakeSendFiles) Delete(context.Context, int64) (*models.EmmaSendFile, error) {
	return nil, repo.ErrNotFound
}

func (f *fakeSendFiles) ListActive(context.Context) ([]models.EmmaSendFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listActiveCalls++
	if f.err != nil {
		return nil, f.err
	}
	var out []models.EmmaSendFile
	for id := int64(0); id < 100; id++ { // стабильный порядок id ASC
		if file, ok := f.files[id]; ok && file.IsActive {
			out = append(out, file)
		}
	}
	return out, nil
}

// fakeEvents — записывает emma_events.
type fakeEvents struct {
	mu     sync.Mutex
	events []models.EmmaEvent
}

func (f *fakeEvents) Create(_ context.Context, ev *models.EmmaEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, *ev)
	return nil
}

func (f *fakeEvents) byType(eventType string) []models.EmmaEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []models.EmmaEvent
	for _, ev := range f.events {
		if ev.EventType == eventType {
			out = append(out, ev)
		}
	}
	return out
}

// --- сборка ---

var (
	pricePDF = models.EmmaSendFile{
		ID: 3, Name: "Прайс 2026",
		Description: "отправь, когда клиент спрашивает подробные цены",
		FilePath:    "/data/emma/files/uuid-price", MimeType: "application/pdf",
		FileSize: 1024, IsActive: true,
	}
	officeJPG = models.EmmaSendFile{
		ID: 5, Name: "Фото офиса", Description: "покажи офис по запросу",
		FilePath: "/data/emma/files/uuid-office", MimeType: "image/jpeg",
		FileSize: 2048, IsActive: true,
	}
)

// newEP04Processor — процессор с библиотекой файлов и журналом событий.
// Лид — СВЕЖАЯ копия testLead: handoff-ветка (EP-05) мутирует режим, общий
// указатель отравил бы остальные тесты пакета.
func newEP04Processor(t *testing.T, files *fakeSendFiles, ai *fakeAI, snd *fakeSender) (*Processor, *fakeMsgs, *fakeEvents) {
	t.Helper()
	msgs := &fakeMsgs{history: []models.Message{
		{LeadID: 7, Direction: models.DirectionInbound, Content: "Сколько стоит?"},
	}}
	events := &fakeEvents{}
	lead := *testLead
	p := NewProcessor(ProcessorDeps{
		Leads:     newFakeLeads(&lead),
		Msgs:      msgs,
		Budgeter:  mustBudgeter(t),
		AI:        ai,
		Sender:    snd,
		FilesProv: NewSendFilesProvider(files, testLogger()),
		SendFiles: files,
		Events:    events,
		Log:       testLogger(),
	})
	return p, msgs, events
}

// --- тесты ---

// Критерий приёмки: фейк Claude отвечает «Вот прайс {{file:N}}» → Send
// получил текст БЕЗ маркера, в messages — чистый текст и [файл: …],
// SendDocument вызван с путём файла N, событие file_sent записано.
func TestEP04_FileMarkerHappyPath(t *testing.T) {
	files := newFakeSendFiles(pricePDF)
	ai := &fakeAI{reply: "Вот прайс. {{file:3}}"}
	snd := &fakeSender{}
	p, msgs, events := newEP04Processor(t, files, ai, snd)

	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	// Секция файлов ушла в system-блок (с описанием и инструкцией маркера).
	sys := ai.lastSystem()
	for _, want := range []string{
		"Тебе доступны файлы для отправки клиенту:",
		"[id=3] Прайс 2026 — отправь, когда клиент спрашивает подробные цены",
		"{{file:3}}",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("в system-блоке нет %q;\nsystem: %s", want, sys)
		}
	}

	// Клиенту ушёл чистый текст, затем документ (по одному сообщению).
	if len(snd.sent) != 1 || snd.sent[0].text != "Вот прайс." {
		t.Errorf("Send: %+v, ждали один чистый текст «Вот прайс.»", snd.sent)
	}
	if len(snd.docs) != 1 || snd.docs[0].path != pricePDF.FilePath ||
		snd.docs[0].chatID != testLead.TelegramUserID {
		t.Errorf("SendDocument: %+v", snd.docs)
	}
	if snd.docs[0].fileName != "Прайс 2026.pdf" {
		t.Errorf("fileName = %q, ждали «Прайс 2026.pdf»", snd.docs[0].fileName)
	}
	if len(snd.photos) != 0 {
		t.Errorf("SendPhoto не должен вызываться для PDF: %+v", snd.photos)
	}

	// В messages — чистый текст и служебный след [файл: …] (author=bot).
	if len(msgs.created) != 2 {
		t.Fatalf("messages: %+v", msgs.created)
	}
	if msgs.created[0].Content != "Вот прайс." {
		t.Errorf("первый outbound: %q", msgs.created[0].Content)
	}
	note := msgs.created[1]
	if note.Content != "[файл: Прайс 2026]" || note.Author == nil || *note.Author != models.AuthorBot {
		t.Errorf("след файла: %+v", note)
	}

	// Событие file_sent с send_file_id и lead_id.
	sentEvents := events.byType(models.EmmaEventFileSent)
	if len(sentEvents) != 1 || sentEvents[0].SendFileID == nil || *sentEvents[0].SendFileID != 3 ||
		sentEvents[0].LeadID == nil || *sentEvents[0].LeadID != 7 {
		t.Errorf("file_sent: %+v", sentEvents)
	}
}

// Критерий приёмки: JPG уходит через SendPhoto, PDF — через SendDocument
// (ветвление по mime), по одному сообщению на файл.
func TestEP04_MimeBranching(t *testing.T) {
	files := newFakeSendFiles(pricePDF, officeJPG)
	ai := &fakeAI{reply: "Прайс и фото. {{file:3}} {{file:5}}"}
	snd := &fakeSender{}
	p, _, events := newEP04Processor(t, files, ai, snd)

	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(snd.docs) != 1 || snd.docs[0].path != pricePDF.FilePath {
		t.Errorf("SendDocument: %+v", snd.docs)
	}
	if len(snd.photos) != 1 || snd.photos[0].path != officeJPG.FilePath {
		t.Errorf("SendPhoto: %+v", snd.photos)
	}
	if got := events.byType(models.EmmaEventFileSent); len(got) != 2 {
		t.Errorf("file_sent: %+v", got)
	}
}

// Критерий приёмки: маркер с неактивным/несуществующим id — вырезан, файл
// не отправлен, задача НЕ упала, в emma_events — error file_not_found.
func TestEP04_InactiveAndMissingFile(t *testing.T) {
	inactive := pricePDF
	inactive.IsActive = false
	files := newFakeSendFiles(inactive)
	ai := &fakeAI{reply: "Сейчас пришлю. {{file:3}} {{file:99}}"}
	snd := &fakeSender{}
	p, msgs, events := newEP04Processor(t, files, ai, snd)

	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("задача упала: %v", err)
	}

	// Текст ушёл чистым, файлов нет.
	if len(snd.sent) != 1 || snd.sent[0].text != "Сейчас пришлю." {
		t.Errorf("Send: %+v", snd.sent)
	}
	if len(snd.docs)+len(snd.photos) != 0 {
		t.Errorf("файлы не должны отправляться: docs %+v photos %+v", snd.docs, snd.photos)
	}
	// Следов [файл: …] нет — только текст.
	if len(msgs.created) != 1 {
		t.Errorf("messages: %+v", msgs.created)
	}
	// Два события error/file_not_found (выключенный и несуществующий).
	errs := events.byType(models.EmmaEventError)
	if len(errs) != 2 {
		t.Fatalf("error-события: %+v", errs)
	}
	for _, ev := range errs {
		if ev.ErrorKind == nil || *ev.ErrorKind != models.EmmaErrFileNotFound {
			t.Errorf("error_kind: %+v", ev)
		}
	}
	if events.byType(models.EmmaEventFileSent) != nil {
		t.Error("file_sent не должно записываться")
	}
}

// {{handoff}} вырезан, текст ушёл чистым одним сообщением; с EP-05 маркер
// уже ОБРАБАТЫВАЕТСЯ (режим human, событие handoff) — контракт EP-04
// «только вырезать» заменён по task EP-05 §7.
func TestEP04_HandoffOnlyStripped(t *testing.T) {
	files := newFakeSendFiles(pricePDF)
	ai := &fakeAI{reply: "Позову менеджера. {{handoff}}"}
	snd := &fakeSender{}
	p, msgs, events := newEP04Processor(t, files, ai, snd)

	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(snd.sent) != 1 || snd.sent[0].text != "Позову менеджера." {
		t.Errorf("Send: %+v", snd.sent)
	}
	if len(msgs.created) != 1 || msgs.created[0].Content != "Позову менеджера." {
		t.Errorf("messages: %+v (confirm-текст не должен дублироваться)", msgs.created)
	}
	// EP-05: маркер переводит диалог менеджеру — событие handoff записано.
	if got := events.byType(models.EmmaEventHandoff); len(got) != 1 {
		t.Errorf("emma_events handoff: %+v", got)
	}
	lead, err := p.deps.Leads.GetByID(context.Background(), 7)
	if err != nil || lead.DialogMode != models.DialogModeHuman {
		t.Errorf("dialog_mode = %q (%v), ждали human", lead.DialogMode, err)
	}
	if len(snd.docs)+len(snd.photos) != 0 {
		t.Errorf("файлы: docs %+v photos %+v", snd.docs, snd.photos)
	}
}

// Критерий приёмки: ошибка Telegram на отправке файла → error(telegram_api)
// в emma_events, задача завершена без ретрая, текст не задублирован.
func TestEP04_TelegramErrorOnFileNoRetry(t *testing.T) {
	files := newFakeSendFiles(pricePDF)
	ai := &fakeAI{reply: "Вот прайс. {{file:3}}"}
	snd := &fakeSender{fileErr: errors.New("telegram: 502")}
	p, msgs, events := newEP04Processor(t, files, ai, snd)

	// nil = задача завершена, Asynq ретраить не будет.
	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("задача не должна ретраиться: %v", err)
	}
	// Текст ушёл ровно один раз, Claude вызван один раз.
	if snd.sentCount() != 1 {
		t.Errorf("Send вызван %d раз", snd.sentCount())
	}
	if ai.callCount() != 1 {
		t.Errorf("Claude вызван %d раз", ai.callCount())
	}
	// След [файл: …] не создан (файл не дошёл), событие error записано.
	if len(msgs.created) != 1 {
		t.Errorf("messages: %+v", msgs.created)
	}
	errs := events.byType(models.EmmaEventError)
	if len(errs) != 1 || errs[0].ErrorKind == nil || *errs[0].ErrorKind != models.EmmaErrTelegramAPI ||
		errs[0].SendFileID == nil || *errs[0].SendFileID != 3 {
		t.Errorf("error-событие: %+v", errs)
	}
	if events.byType(models.EmmaEventFileSent) != nil {
		t.Error("file_sent не должно записываться")
	}
}

// Ответ из одних маркеров: текст не отправляется (Telegram пустой Send не
// примет), файл уходит, след [файл: …] остаётся единственным outbound.
func TestEP04_MarkerOnlyReply(t *testing.T) {
	files := newFakeSendFiles(pricePDF)
	ai := &fakeAI{reply: "{{file:3}}"}
	snd := &fakeSender{}
	p, msgs, _ := newEP04Processor(t, files, ai, snd)

	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if snd.sentCount() != 0 {
		t.Errorf("пустой текст не должен отправляться: %+v", snd.sent)
	}
	if len(snd.docs) != 1 {
		t.Errorf("SendDocument: %+v", snd.docs)
	}
	if len(msgs.created) != 1 || msgs.created[0].Content != "[файл: Прайс 2026]" {
		t.Errorf("messages: %+v", msgs.created)
	}
}

// Без маркеров ничего не меняется (регресс M3): один Send, один outbound.
func TestEP04_NoMarkersNoOp(t *testing.T) {
	files := newFakeSendFiles(pricePDF)
	ai := &fakeAI{reply: "Просто ответ без файлов."}
	snd := &fakeSender{}
	p, msgs, events := newEP04Processor(t, files, ai, snd)

	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if snd.sentCount() != 1 || len(snd.docs)+len(snd.photos) != 0 {
		t.Errorf("send: %+v docs %+v photos %+v", snd.sent, snd.docs, snd.photos)
	}
	if len(msgs.created) != 1 || len(events.events) != 0 {
		t.Errorf("messages %+v, события %+v", msgs.created, events.events)
	}
}

// --- секция промпта и кэш 30 с ---

// Критерий приёмки: PATCH is_active=false → файл исчезает из секции ≤30 с
// (кэш провайдера), пустой список → секции нет вовсе.
func TestSendFilesSectionAndCacheTTL(t *testing.T) {
	if got := sendFilesSection(nil); got != "" {
		t.Fatalf("пустой список должен давать пустую секцию: %q", got)
	}

	files := newFakeSendFiles(pricePDF, officeJPG)
	prov := NewSendFilesProvider(files, testLogger())
	now := time.Unix(1700000000, 0)
	prov.now = func() time.Time { return now }
	ctx := context.Background()

	first, err := prov.Active(ctx)
	if err != nil || len(first) != 2 {
		t.Fatalf("active: %v, %v", first, err)
	}
	section := sendFilesSection(first)
	for _, want := range []string{"[id=3] Прайс 2026", "[id=5] Фото офиса", "{{file:3}}"} {
		if !strings.Contains(section, want) {
			t.Errorf("в секции нет %q:\n%s", want, section)
		}
	}

	// Файл выключили (PATCH is_active=false) — внутри TTL отдаётся кэш…
	files.mu.Lock()
	inactive := files.files[3]
	inactive.IsActive = false
	files.files[3] = inactive
	files.mu.Unlock()

	now = now.Add(29 * time.Second)
	cached, _ := prov.Active(ctx)
	if len(cached) != 2 || files.listActiveCalls != 1 {
		t.Fatalf("внутри TTL ждали кэш (1 чтение БД): файлов %d, чтений %d",
			len(cached), files.listActiveCalls)
	}

	// …а после истечения TTL (≤30 с) файл исчезает из секции.
	now = now.Add(2 * time.Second)
	fresh, _ := prov.Active(ctx)
	if len(fresh) != 1 || fresh[0].ID != 5 {
		t.Fatalf("после TTL: %+v", fresh)
	}
	if got := sendFilesSection(fresh); strings.Contains(got, "Прайс 2026") {
		t.Errorf("выключенный файл остался в секции:\n%s", got)
	}

	// Ошибка БД деградирует до последнего удачного списка, не до ошибки.
	files.mu.Lock()
	files.err = errors.New("db down")
	files.mu.Unlock()
	now = now.Add(time.Minute)
	stale, err := prov.Active(ctx)
	if err != nil || len(stale) != 1 {
		t.Fatalf("деградация на протухший кэш: %v, %v", stale, err)
	}
}

// Валидация маркера идёт МИМО кэша секции: выключенный файл перестаёт
// проходить сразу, даже пока секция ещё в кэше (критерий приёмки).
func TestEP04_ValidationBypassesCache(t *testing.T) {
	files := newFakeSendFiles(pricePDF)
	ai := &fakeAI{reply: "Вот прайс. {{file:3}}"}
	snd := &fakeSender{}
	p, _, events := newEP04Processor(t, files, ai, snd)

	// Прогрев кэша секции — файл ещё активен.
	if _, err := p.deps.FilesProv.Active(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Файл выключают.
	files.mu.Lock()
	inactive := files.files[3]
	inactive.IsActive = false
	files.files[3] = inactive
	files.mu.Unlock()

	if err := p.HandleProcessInbound(context.Background(), inboundTask(t, 7, 100)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(snd.docs) != 0 {
		t.Errorf("выключенный файл отправлен: %+v", snd.docs)
	}
	errs := events.byType(models.EmmaEventError)
	if len(errs) != 1 || *errs[0].ErrorKind != models.EmmaErrFileNotFound {
		t.Errorf("error-событие: %+v", errs)
	}
}
