package repo

import (
	"context"
	"testing"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

// M14: CreateInboundSetLanguage — язык пишется той же транзакцией, что и
// сообщение, ровно один раз (guard WHERE language IS NULL).
func TestCreateInboundSetLanguage_OnceOnly(t *testing.T) {
	leads, msgs, _ := testRepos(t)
	ctx := context.Background()

	lead := &models.Lead{TelegramUserID: 555}
	if err := leads.Create(ctx, lead); err != nil {
		t.Fatal(err)
	}
	got, err := leads.GetByID(ctx, lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Language != nil {
		t.Fatalf("новый лид обязан быть без языка, получили %q", *got.Language)
	}

	// Первый текстовый inbound: язык записан, langSet=true.
	set, err := msgs.CreateInboundSetLanguage(ctx,
		&models.Message{LeadID: lead.ID, Content: "Hola"}, "es")
	if err != nil {
		t.Fatal(err)
	}
	if !set {
		t.Fatal("первый вызов обязан записать язык (langSet=true)")
	}
	got, err = leads.GetByID(ctx, lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Language == nil || *got.Language != "es" {
		t.Fatalf("language = %v, ждали es", got.Language)
	}
	if got.MessageCount != 1 {
		t.Errorf("message_count = %d, ждали 1 (счётчики двигаются как у CreateInbound)", got.MessageCount)
	}

	// Повтор (гонка/ретрай): сообщение сохраняется, язык НЕ перетирается.
	set, err = msgs.CreateInboundSetLanguage(ctx,
		&models.Message{LeadID: lead.ID, Content: "Hi"}, "en")
	if err != nil {
		t.Fatal(err)
	}
	if set {
		t.Fatal("повторный вызов не должен перетирать язык (langSet=false)")
	}
	got, err = leads.GetByID(ctx, lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Language == nil || *got.Language != "es" {
		t.Fatalf("после повтора language = %v, ждали es", got.Language)
	}
	if got.MessageCount != 2 {
		t.Errorf("message_count = %d, ждали 2 (второе сообщение сохранено)", got.MessageCount)
	}

	// Ручная смена менеджером — UpdateFields, CHECK пускает только тройку.
	if err := leads.UpdateFields(ctx, lead.ID, map[string]interface{}{"language": "ru"}); err != nil {
		t.Fatal(err)
	}
	if err := leads.UpdateFields(ctx, lead.ID, map[string]interface{}{"language": "pt"}); err == nil {
		t.Error("CHECK (language IN ru/en/es) обязан отвергать pt")
	}
}
