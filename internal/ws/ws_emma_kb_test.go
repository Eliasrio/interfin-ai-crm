// EP-03: событие emma_kb_status доезжает до WS-клиента через боевой
// контур events.RedisPublisher → Redis pub/sub → Hub (критерий приёмки;
// по образцу TestStageChange_DeliveredUnder500ms). Требует REDIS_TEST_ADDR.
package ws

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/interfin/interfin-ai-crm/internal/auth"
	"github.com/interfin/interfin-ai-crm/internal/events"
	"github.com/interfin/interfin-ai-crm/internal/models"
)

func TestEmmaKBStatus_DeliveredToClient(t *testing.T) {
	addr := testRedisAddr(t)
	r := newRig(t, DefaultTimings)
	_, pub := runHub(t, r, addr)

	conn, _, err := r.dial(r.token(t, time.Minute, auth.RoleAdmin))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	r.waitClients(t, 1)
	rd := readLoop(conn)
	warmup(t, pub, rd)

	errText := "PDF без текстового слоя, OCR не поддерживается"
	if err := pub.Publish(context.Background(),
		events.EmmaKBStatusEvent(7, "прайс.pdf", models.EmmaKBError, 0, &errText)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Warmup-события могли остаться в буфере — вычитываем до нашего типа.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var got events.Event
		if err := json.Unmarshal(rd.next(t, 3*time.Second), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Type != events.TypeEmmaKBStatus {
			continue // прогревочный stage_change
		}
		if got.FileID != 7 || got.Filename != "прайс.pdf" ||
			got.KBStatus != models.EmmaKBError || got.KBError != errText {
			t.Fatalf("событие исказилось: %+v", got)
		}
		if got.TS.IsZero() {
			t.Fatal("TS не проставлен (catch-up §10.3 без него слепнет)")
		}
		return
	}
	t.Fatal("emma_kb_status так и не пришло клиенту")
}
