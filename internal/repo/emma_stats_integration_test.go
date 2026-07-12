// Интеграционные тесты EP-06 против реального PostgreSQL
// (POSTGRES_TEST_DSN, иначе skip). Критерий приёмки: числа GET /stats
// сходятся с посчитанными руками на фикстурах — ответы, avg/p95, токены,
// handoff, файлы по каждому, активные диалоги за 24 ч; журнал ошибок —
// фильтр по kind, пагинация 50, корректный total.
package repo

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

// mkEvent — событие с заданным created_at (GORM использует непустое поле
// как есть, дефолт БД не срабатывает).
func mkEvent(t *testing.T, gdb *gorm.DB, ev models.EmmaEvent, at time.Time) {
	t.Helper()
	ev.CreatedAt = at
	if err := gdb.Create(&ev).Error; err != nil {
		t.Fatalf("emma_event: %v", err)
	}
}

func iptr(v int) *int { return &v }

func TestEmmaStats_NumbersAddUp(t *testing.T) {
	gdb := testEmmaDB(t)
	statsRepo := NewEmmaStats(gdb)
	ctx := context.Background()
	now := time.Now().UTC()
	in := func(min int) time.Time { return now.Add(-time.Duration(min) * time.Minute) }

	// Два файла библиотеки — разбивка «по каждому» идёт через JOIN имён.
	price := models.EmmaSendFile{Name: "Прайс 2026", Description: "цены",
		FilePath: "/x/a", MimeType: "application/pdf", FileSize: 1, IsActive: true}
	photo := models.EmmaSendFile{Name: "Фото офиса", Description: "офис",
		FilePath: "/x/b", MimeType: "image/jpeg", FileSize: 1, IsActive: true}
	if err := gdb.Create(&price).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&photo).Error; err != nil {
		t.Fatal(err)
	}

	// Ответы в периоде: времена 100/200/300/400 мс → avg 250;
	// percentile_cont(0.95): 300 + 0.85×100 = 385. Токены: in 10+20+30+40,
	// out 1+2+3+4.
	for i, rt := range []int{100, 200, 300, 400} {
		mkEvent(t, gdb, models.EmmaEvent{
			EventType: models.EmmaEventReply, ResponseTimeMs: iptr(rt),
			TokensIn: iptr((i + 1) * 10), TokensOut: iptr(i + 1),
		}, in(10+i))
	}
	// Ответ ВНЕ периода (старше часа) — не должен попасть в счёт.
	mkEvent(t, gdb, models.EmmaEvent{
		EventType: models.EmmaEventReply, ResponseTimeMs: iptr(9999),
		TokensIn: iptr(1000), TokensOut: iptr(1000),
	}, in(120))

	// Handoff ×2 в периоде, файлы: Прайс ×3, Фото ×1, удалённый (NULL) ×1.
	mkEvent(t, gdb, models.EmmaEvent{EventType: models.EmmaEventHandoff}, in(5))
	mkEvent(t, gdb, models.EmmaEvent{EventType: models.EmmaEventHandoff}, in(6))
	for i := 0; i < 3; i++ {
		mkEvent(t, gdb, models.EmmaEvent{
			EventType: models.EmmaEventFileSent, SendFileID: &price.ID,
		}, in(7+i))
	}
	mkEvent(t, gdb, models.EmmaEvent{
		EventType: models.EmmaEventFileSent, SendFileID: &photo.ID,
	}, in(11))
	mkEvent(t, gdb, models.EmmaEvent{EventType: models.EmmaEventFileSent}, in(12))

	// Ошибки: llm_api ×2 и timeout ×1 в периоде, kb_index ×1 вне периода.
	llm, tmo, kbi := models.EmmaErrLLMAPI, models.EmmaErrTimeout, models.EmmaErrKBIndex
	mkEvent(t, gdb, models.EmmaEvent{EventType: models.EmmaEventError, ErrorKind: &llm}, in(3))
	mkEvent(t, gdb, models.EmmaEvent{EventType: models.EmmaEventError, ErrorKind: &llm}, in(4))
	mkEvent(t, gdb, models.EmmaEvent{EventType: models.EmmaEventError, ErrorKind: &tmo}, in(5))
	mkEvent(t, gdb, models.EmmaEvent{EventType: models.EmmaEventError, ErrorKind: &kbi}, in(180))

	got, err := statsRepo.Stats(ctx, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if got.Replies != 4 {
		t.Errorf("replies = %d, ждали 4 (событие вне периода не в счёт)", got.Replies)
	}
	if got.AvgResponseMs != 250 {
		t.Errorf("avg = %v, ждали 250", got.AvgResponseMs)
	}
	if got.P95ResponseMs != 385 {
		t.Errorf("p95 = %v, ждали 385 (percentile_cont на 100/200/300/400)", got.P95ResponseMs)
	}
	if got.TokensIn != 100 || got.TokensOut != 10 {
		t.Errorf("токены = %d/%d, ждали 100/10", got.TokensIn, got.TokensOut)
	}
	if got.Handoffs != 2 {
		t.Errorf("handoffs = %d, ждали 2", got.Handoffs)
	}
	if got.FilesSent != 5 {
		t.Errorf("files_sent = %d, ждали 5", got.FilesSent)
	}
	if len(got.Files) != 3 {
		t.Fatalf("разбивка файлов: %+v", got.Files)
	}
	// Порядок: count DESC → Прайс(3) первым; NULL-строка с заглушкой имени.
	if got.Files[0].Name != "Прайс 2026" || got.Files[0].Count != 3 ||
		got.Files[0].SendFileID == nil || *got.Files[0].SendFileID != price.ID {
		t.Errorf("files[0] = %+v", got.Files[0])
	}
	var nullRow *EmmaFileSentCount
	for i := range got.Files {
		if got.Files[i].SendFileID == nil {
			nullRow = &got.Files[i]
		}
	}
	if nullRow == nil || nullRow.Count != 1 || nullRow.Name != "(файл удалён)" {
		t.Errorf("строка удалённого файла: %+v", nullRow)
	}
	if got.ErrorsByKind[models.EmmaErrLLMAPI] != 2 || got.ErrorsByKind[models.EmmaErrTimeout] != 1 {
		t.Errorf("ошибки: %v", got.ErrorsByKind)
	}
	if _, ok := got.ErrorsByKind[models.EmmaErrKBIndex]; ok {
		t.Errorf("kb_index вне периода не должен считаться: %v", got.ErrorsByKind)
	}

	// Пустой период — нули, не NULL-ошибки сканирования.
	empty, err := statsRepo.Stats(ctx, now.Add(-time.Minute), now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("пустой период: %v", err)
	}
	if empty.Replies != 0 || empty.TokensIn != 0 || empty.AvgResponseMs != 0 || empty.P95ResponseMs != 0 {
		t.Errorf("пустой период: %+v", empty)
	}
}

// Критерий приёмки: фильтр по kind работает, пагинация 50, total корректен,
// новые сверху.
func TestEmmaStats_ListErrors(t *testing.T) {
	gdb := testEmmaDB(t)
	statsRepo := NewEmmaStats(gdb)
	ctx := context.Background()
	now := time.Now().UTC()

	llm, tg := models.EmmaErrLLMAPI, models.EmmaErrTelegramAPI
	// 55 llm_api (новейшая — самая последняя по времени) + 5 telegram_api.
	// lead_id не задаётся: FK на leads, а лидов в фикстуре нет (в бою после
	// erasure lead_id тоже NULL — SET NULL).
	for i := 0; i < 55; i++ {
		d := fmt.Sprintf("llm-%02d", i)
		mkEvent(t, gdb, models.EmmaEvent{
			EventType: models.EmmaEventError, ErrorKind: &llm, Detail: &d,
		}, now.Add(-time.Duration(55-i)*time.Minute)) // i=54 — новейшая
	}
	for i := 0; i < 5; i++ {
		mkEvent(t, gdb, models.EmmaEvent{
			EventType: models.EmmaEventError, ErrorKind: &tg,
		}, now.Add(-time.Duration(200+i)*time.Minute))
	}
	// Не-error события в журнал не попадают.
	mkEvent(t, gdb, models.EmmaEvent{EventType: models.EmmaEventReply, ResponseTimeMs: iptr(1)}, now)

	// Без фильтра: total 60, страница 1 — 50 строк, новые сверху.
	page1, total, err := statsRepo.ListErrors(ctx, "", time.Time{}, now.Add(time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	if total != 60 || len(page1) != 50 {
		t.Fatalf("total=%d len=%d, ждали 60/50", total, len(page1))
	}
	if page1[0].ErrorKind == nil || *page1[0].ErrorKind != llm ||
		page1[0].Detail == nil || *page1[0].Detail != "llm-54" {
		t.Errorf("первой должна идти новейшая llm-запись (llm-54): %+v", page1[0])
	}
	if !page1[0].CreatedAt.After(page1[49].CreatedAt) {
		t.Error("порядок не «новые сверху»")
	}

	page2, total, err := statsRepo.ListErrors(ctx, "", time.Time{}, now.Add(time.Minute), 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 60 || len(page2) != 10 {
		t.Errorf("страница 2: total=%d len=%d, ждали 60/10", total, len(page2))
	}

	// Фильтр по kind.
	tgRows, tgTotal, err := statsRepo.ListErrors(ctx, tg, time.Time{}, now.Add(time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	if tgTotal != 5 || len(tgRows) != 5 {
		t.Errorf("telegram_api: total=%d len=%d, ждали 5/5", tgTotal, len(tgRows))
	}
	for _, r := range tgRows {
		if r.ErrorKind == nil || *r.ErrorKind != tg {
			t.Errorf("чужой kind в фильтре: %+v", r)
		}
	}

	// Фильтр по периоду: llm-события старше 30 минут отсекаются.
	recent, recentTotal, err := statsRepo.ListErrors(ctx, llm, now.Add(-30*time.Minute), now.Add(time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	if recentTotal != 30 || len(recent) != 30 {
		// created = now-(55-i)m; фильтр created_at >= now-30m включительно:
		// 55-i ≤ 30 → i ≥ 25 → i=25..54 = 30 строк.
		t.Errorf("период: total=%d len=%d, ждали 30", recentTotal, len(recent))
	}
}

// Метрики из leads/messages: новые лиды и сообщения за период, активные
// диалоги — distinct lead_id по inbound строго за последние 24 ч.
func TestEmmaStats_DialogStats(t *testing.T) {
	gdb := testEmmaDB(t)
	statsRepo := NewEmmaStats(gdb)
	ctx := context.Background()
	now := time.Now().UTC()

	mkLeadAt := func(tgID int64, at time.Time) *models.Lead {
		lead := &models.Lead{TelegramUserID: tgID, StageID: 1, CreatedAt: at, LastActivityAt: at}
		if err := gdb.Create(lead).Error; err != nil {
			t.Fatal(err)
		}
		return lead
	}
	mkMsg := func(leadID int64, dir string, at time.Time) {
		m := &models.Message{LeadID: leadID, Direction: dir, Content: "x", CreatedAt: at}
		if err := gdb.Create(m).Error; err != nil {
			t.Fatal(err)
		}
	}

	l1 := mkLeadAt(1001, now.Add(-2*time.Hour))  // в периоде
	l2 := mkLeadAt(1002, now.Add(-30*time.Hour)) // вне периода 24ч, в периоде 48ч
	l3 := mkLeadAt(1003, now.Add(-100*time.Hour))

	// l1: 3 inbound за последний час + 2 outbound.
	for i := 0; i < 3; i++ {
		mkMsg(l1.ID, models.DirectionInbound, now.Add(-time.Duration(i+1)*time.Minute))
	}
	mkMsg(l1.ID, models.DirectionOutbound, now.Add(-time.Minute))
	mkMsg(l1.ID, models.DirectionOutbound, now.Add(-2*time.Minute))
	// l2: inbound 30 часов назад — активным (24 ч) не считается.
	mkMsg(l2.ID, models.DirectionInbound, now.Add(-30*time.Hour))
	// l3: только outbound недавно — активным не считается (считаем inbound).
	mkMsg(l3.ID, models.DirectionOutbound, now.Add(-time.Minute))

	from := now.Add(-48 * time.Hour)
	got, err := statsRepo.DialogStats(ctx, from, now.Add(time.Minute), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.NewLeads != 2 {
		t.Errorf("new_leads = %d, ждали 2 (l1+l2; l3 старше 48ч)", got.NewLeads)
	}
	if got.ActiveDialogs != 1 {
		t.Errorf("active_dialogs = %d, ждали 1 (только l1: inbound за 24ч)", got.ActiveDialogs)
	}
	if got.MessagesIn != 4 {
		t.Errorf("messages_in = %d, ждали 4 (3 у l1 + 1 у l2 в периоде 48ч)", got.MessagesIn)
	}
	if got.MessagesOut != 3 {
		t.Errorf("messages_out = %d, ждали 3", got.MessagesOut)
	}
}
