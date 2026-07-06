package repo

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

// New создаёт все три репозитория поверх одного *gorm.DB
// (открытого через internal/db — см. правило SimpleProtocol §4.2).
func New(db *gorm.DB) (LeadRepo, MessageRepo, PaymentRepo) {
	return &leadRepo{db: db}, &messageRepo{db: db}, &paymentRepo{db: db}
}

func wrapNotFound(err error, what string) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// --- leads ---

type leadRepo struct{ db *gorm.DB }

func (r *leadRepo) Create(ctx context.Context, lead *models.Lead) error {
	if err := r.db.WithContext(ctx).Create(lead).Error; err != nil {
		return fmt.Errorf("repo: create lead: %w", err)
	}
	return nil
}

func (r *leadRepo) GetByID(ctx context.Context, id int64) (*models.Lead, error) {
	var lead models.Lead
	if err := r.db.WithContext(ctx).First(&lead, id).Error; err != nil {
		return nil, wrapNotFound(err, "repo: get lead by id")
	}
	return &lead, nil
}

func (r *leadRepo) GetByTelegramUserID(ctx context.Context, tgUserID int64) (*models.Lead, error) {
	var lead models.Lead
	err := r.db.WithContext(ctx).
		Where("telegram_user_id = ?", tgUserID).
		First(&lead).Error
	if err != nil {
		return nil, wrapNotFound(err, "repo: get lead by telegram_user_id")
	}
	return &lead, nil
}

func (r *leadRepo) Save(ctx context.Context, lead *models.Lead) error {
	if lead.ID == 0 {
		return errors.New("repo: save lead: пустой ID")
	}
	if err := r.db.WithContext(ctx).Save(lead).Error; err != nil {
		return fmt.Errorf("repo: save lead: %w", err)
	}
	return nil
}

func (r *leadRepo) UpdateFields(ctx context.Context, id int64, fields map[string]interface{}) error {
	res := r.db.WithContext(ctx).
		Model(&models.Lead{}).
		Where("id = ?", id).
		Updates(fields)
	if res.Error != nil {
		return fmt.Errorf("repo: update lead fields: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("repo: update lead fields: lead %d: %w", id, ErrNotFound)
	}
	return nil
}

func (r *leadRepo) TransitionStage(ctx context.Context, id int64, from, to int16) (bool, error) {
	// Один UPDATE: смена стадии и сброс per-stage счётчика anti-spam (§3.5)
	// неразделимы — раздельные запросы оставили бы окно, где лид уже в новой
	// стадии со старым счётчиком. Guard по from — приоритет §3.1: проигравший
	// гонку переход не перетирает победителя, а получает false.
	// last_activity_at двигается той же командой: TTL новой стадии взводится
	// сразу после перехода и обязан считаться от той же точки, что и поле
	// (CLAUDE.md §4.7); заодно catch-up ?updated_since (§10.3, M8/M9) увидит
	// переходы, случившиеся без сообщений лида (ручные, TTL, payment).
	res := r.db.WithContext(ctx).
		Model(&models.Lead{}).
		Where("id = ? AND stage_id = ?", id, from).
		Updates(map[string]interface{}{
			"stage_id":         to,
			"anti_spam_count":  0,
			"last_activity_at": gorm.Expr("NOW()"),
		})
	if res.Error != nil {
		return false, fmt.Errorf("repo: transition stage: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// --- messages ---

type messageRepo struct{ db *gorm.DB }

func (r *messageRepo) CreateInbound(ctx context.Context, msg *models.Message) error {
	msg.Direction = models.DirectionInbound
	// Транзакция: сообщение и счётчики лида меняются атомарно, инкремент —
	// выражением в SQL, а не read-modify-write (конкурентные inbound не теряются).
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(msg).Error; err != nil {
			return fmt.Errorf("insert message: %w", err)
		}
		res := tx.Model(&models.Lead{}).
			Where("id = ?", msg.LeadID).
			Updates(map[string]interface{}{
				"message_count":    gorm.Expr("message_count + 1"),   // только inbound (CLAUDE.md §4.3)
				"anti_spam_count":  gorm.Expr("anti_spam_count + 1"), // per-stage, сброс в M5
				"last_activity_at": gorm.Expr("NOW()"),               // от него считается TTL (CLAUDE.md §4.7)
			})
		if res.Error != nil {
			return fmt.Errorf("bump lead counters: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("bump lead counters: lead %d: %w", msg.LeadID, ErrNotFound)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("repo: create inbound message: %w", err)
	}
	return nil
}

func (r *messageRepo) CreateOutbound(ctx context.Context, msg *models.Message) error {
	msg.Direction = models.DirectionOutbound
	// Ответы бота message_count НЕ увеличивают (CLAUDE.md §4.3).
	if err := r.db.WithContext(ctx).Create(msg).Error; err != nil {
		return fmt.Errorf("repo: create outbound message: %w", err)
	}
	return nil
}

func (r *messageRepo) ListByLead(ctx context.Context, leadID int64, limit int) ([]models.Message, error) {
	// Подзапросом берём последние limit, снаружи разворачиваем к порядку
	// «старые → новые» — так история уходит в контекст Claude.
	sub := r.db.WithContext(ctx).
		Model(&models.Message{}).
		Where("lead_id = ?", leadID).
		Order("id DESC")
	if limit > 0 {
		sub = sub.Limit(limit)
	}
	var msgs []models.Message
	err := r.db.WithContext(ctx).
		Table("(?) AS last_messages", sub).
		Order("id ASC").
		Find(&msgs).Error
	if err != nil {
		return nil, fmt.Errorf("repo: list messages: %w", err)
	}
	return msgs, nil
}

// --- payment_events ---

type paymentRepo struct{ db *gorm.DB }

func (r *paymentRepo) Create(ctx context.Context, ev *models.PaymentEvent) error {
	if err := r.db.WithContext(ctx).Create(ev).Error; err != nil {
		return fmt.Errorf("repo: create payment event: %w", err)
	}
	return nil
}

func (r *paymentRepo) ListByLead(ctx context.Context, leadID int64) ([]models.PaymentEvent, error) {
	var evs []models.PaymentEvent
	err := r.db.WithContext(ctx).
		Where("lead_id = ?", leadID).
		Order("id ASC").
		Find(&evs).Error
	if err != nil {
		return nil, fmt.Errorf("repo: list payment events: %w", err)
	}
	return evs, nil
}
