package repo

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

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

func (r *leadRepo) ListPendingTask(ctx context.Context, limit int) ([]models.Lead, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("repo: list pending task: limit %d невалиден", limit)
	}
	var leads []models.Lead
	// WHERE pending_task = TRUE попадает в частичный индекс idx_leads_pending
	// (0003). id ASC — лиды, ждущие дольше всех, восстанавливаются первыми.
	err := r.db.WithContext(ctx).
		Where("pending_task = TRUE").
		Order("id ASC").
		Limit(limit).
		Find(&leads).Error
	if err != nil {
		return nil, fmt.Errorf("repo: list pending task: %w", err)
	}
	return leads, nil
}

func (r *leadRepo) ClearPendingTask(ctx context.Context, id int64, seenMessageCount int) (bool, error) {
	// Guard по message_count — CAS против гонки с вебхуком (§11.2): между
	// нашим enqueue и этим UPDATE лид мог прислать новое сообщение, а его
	// enqueue — снова упасть с pending_task=TRUE. Снятие флага без guard'а
	// потеряло бы то сообщение до следующего inbound.
	res := r.db.WithContext(ctx).
		Model(&models.Lead{}).
		Where("id = ? AND pending_task = TRUE AND message_count = ?", id, seenMessageCount).
		Update("pending_task", false)
	if res.Error != nil {
		return false, fmt.Errorf("repo: clear pending task: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

func (r *leadRepo) List(ctx context.Context, p ListLeadsParams) ([]models.Lead, int64, error) {
	if p.Limit <= 0 {
		return nil, 0, fmt.Errorf("repo: list leads: limit %d невалиден", p.Limit)
	}
	// Стёртых (deleted_at) исключает soft-delete-скоуп GORM сам.
	q := r.db.WithContext(ctx).Model(&models.Lead{})
	if p.UpdatedSince != nil {
		// §10.3: строгое «позже» — события с ts == updated_since клиент уже
		// видел. Фильтр и сортировка ложатся на idx_leads_activity.
		q = q.Where("last_activity_at > ?", *p.UpdatedSince)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("repo: list leads: count: %w", err)
	}
	var leads []models.Lead
	// id DESC вторым ключом: детерминированный порядок при равных
	// last_activity_at — страницы не дублируют и не теряют строки.
	err := q.Order("last_activity_at DESC, id DESC").
		Limit(p.Limit).Offset(p.Offset).
		Find(&leads).Error
	if err != nil {
		return nil, 0, fmt.Errorf("repo: list leads: %w", err)
	}
	return leads, total, nil
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
	return r.listPage(ctx, leadID, 0, limit, "repo: list messages")
}

// ListByLeadBefore — страница чата M12: те же «последние limit, старые →
// новые», но от точки beforeID (прокрутка истории вверх).
func (r *messageRepo) ListByLeadBefore(ctx context.Context, leadID, beforeID int64, limit int) ([]models.Message, error) {
	return r.listPage(ctx, leadID, beforeID, limit, "repo: list messages before")
}

func (r *messageRepo) listPage(ctx context.Context, leadID, beforeID int64, limit int, what string) ([]models.Message, error) {
	// Подзапросом берём последние limit, снаружи разворачиваем к порядку
	// «старые → новые» — так история уходит в контекст Claude и в чат M12.
	sub := r.db.WithContext(ctx).
		Model(&models.Message{}).
		Where("lead_id = ?", leadID).
		Order("id DESC")
	if beforeID > 0 {
		sub = sub.Where("id < ?", beforeID)
	}
	if limit > 0 {
		sub = sub.Limit(limit)
	}
	var msgs []models.Message
	err := r.db.WithContext(ctx).
		Table("(?) AS last_messages", sub).
		Order("id ASC").
		Find(&msgs).Error
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return msgs, nil
}

func (r *messageRepo) HasManagerOutboundAfter(ctx context.Context, leadID, afterID int64) (bool, error) {
	var found models.Message
	err := r.db.WithContext(ctx).
		Select("id").
		Where("lead_id = ? AND id > ? AND direction = ? AND author LIKE 'manager:%'",
			leadID, afterID, models.DirectionOutbound).
		First(&found).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("repo: has manager outbound after: %w", err)
	}
	return true, nil
}

// --- settings (M13) ---

// NewSettings — репозиторий settings поверх того же *gorm.DB.
func NewSettings(db *gorm.DB) SettingsRepo {
	return &settingsRepo{db: db}
}

type settingsRepo struct{ db *gorm.DB }

func (r *settingsRepo) Get(ctx context.Context, key string) (string, error) {
	var s models.Setting
	if err := r.db.WithContext(ctx).First(&s, "key = ?", key).Error; err != nil {
		return "", wrapNotFound(err, "repo: get setting")
	}
	return s.Value, nil
}

func (r *settingsRepo) Set(ctx context.Context, key, value string) error {
	err := r.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "key"}},
			DoUpdates: clause.Assignments(map[string]interface{}{
				"value":      value,
				"updated_at": gorm.Expr("NOW()"),
			}),
		}).
		Create(&models.Setting{Key: key, Value: value}).Error
	if err != nil {
		return fmt.Errorf("repo: set setting %s: %w", key, err)
	}
	return nil
}

// --- payment_events ---

type paymentRepo struct{ db *gorm.DB }

// Create идемпотентен по (gateway, raw_payload->>'update_id') — уникальный
// индекс 0008: ретрай платёжного вебхука (nonce возвращён после провала
// перехода, M6) не плодит вторую фискальную запись. Повтор = no-op без ошибки.
func (r *paymentRepo) Create(ctx context.Context, ev *models.PaymentEvent) error {
	err := r.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(ev).Error
	if err != nil {
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
