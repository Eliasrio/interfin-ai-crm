// lgpd.go — LGPDRepo (M8, SRS §9): erasure, export-выборки, retention.
//
// Операции намеренно живут отдельно от leadRepo: они единственные, кому
// позволено обходить soft-delete-скоуп GORM (Unscoped) и трогать стёртых
// лидов. Бизнес-код ходит через LeadRepo и стёртых не видит вовсе.
package repo

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/interfin/interfin-ai-crm/internal/lgpd"
	"github.com/interfin/interfin-ai-crm/internal/models"
)

// NewLGPD — LGPDRepo поверх того же *gorm.DB, что и остальные репозитории.
func NewLGPD(db *gorm.DB) LGPDRepo {
	return &lgpdRepo{db: db}
}

type lgpdRepo struct{ db *gorm.DB }

// Erase — «право на забвение» §9.1/§9.3 одной транзакцией. Guard
// deleted_at IS NULL (его добавляет soft-delete-скоуп GORM) даёт
// идемпотентность: повторный erase того же лида → ErrNotFound, хеш
// не перезатирается и вторая audit-запись не появляется.
func (r *lgpdRepo) Erase(ctx context.Context, leadID int64, hashedTgID int64, performedBy, ip string) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&models.Lead{}).
			Where("id = ?", leadID). // скоуп GORM сам добавит deleted_at IS NULL
			Updates(map[string]interface{}{
				"deleted_at":  gorm.Expr("NOW()"),
				"name":        nil,
				"phone":       nil,
				"tg_username": nil,
				// UNIQUE NOT NULL — хеш, не NULL (§9.3, AQ²-4, CLAUDE.md §4.8)
				"telegram_user_id": hashedTgID,
			})
		if res.Error != nil {
			return fmt.Errorf("anonymize lead: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("lead %d не найден или уже стёрт: %w", leadID, ErrNotFound)
		}

		err := tx.Model(&models.Message{}).
			Where("lead_id = ?", leadID).
			Update("content", lgpd.DeletedContent).Error
		if err != nil {
			return fmt.Errorf("wipe messages: %w", err)
		}
		// payment_events НЕ трогаем: фискальная retention 5 лет (§9.3).

		if err := createAudit(tx, lgpd.ActionErase, leadID, performedBy, ip); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("repo: lgpd erase: %w", err)
	}
	return nil
}

func (r *lgpdRepo) GetLeadAny(ctx context.Context, id int64) (*models.Lead, error) {
	var lead models.Lead
	if err := r.db.WithContext(ctx).Unscoped().First(&lead, id).Error; err != nil {
		return nil, wrapNotFound(err, "repo: lgpd get lead")
	}
	return &lead, nil
}

func (r *lgpdRepo) CreateAudit(ctx context.Context, a *models.LGPDAudit) error {
	if err := r.db.WithContext(ctx).Create(a).Error; err != nil {
		return fmt.Errorf("repo: lgpd create audit: %w", err)
	}
	return nil
}

func (r *lgpdRepo) ListAuditByLead(ctx context.Context, leadID int64) ([]models.LGPDAudit, error) {
	var rows []models.LGPDAudit
	err := r.db.WithContext(ctx).
		Where("lead_id = ?", leadID).
		Order("id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("repo: lgpd list audit: %w", err)
	}
	return rows, nil
}

// DeleteErasedBefore — retention §9.1: физическое удаление лидов, стёртых
// раньше cutoff. messages уходят каскадом FK; payment_events выживают
// с lead_id=NULL (FK SET NULL, 0010); rag_audit/lgpd_audit без FK — след
// операций сохраняется.
func (r *lgpdRepo) DeleteErasedBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Unscoped().
		Where("deleted_at IS NOT NULL AND deleted_at < ?", cutoff).
		Delete(&models.Lead{})
	if res.Error != nil {
		return 0, fmt.Errorf("repo: lgpd retention delete: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// createAudit — вставка §9.2 внутри уже открытой транзакции.
func createAudit(tx *gorm.DB, action string, leadID int64, performedBy, ip string) error {
	a := &models.LGPDAudit{
		Action:      action,
		LeadID:      &leadID,
		PerformedBy: performedBy,
	}
	if ip != "" {
		a.IPAddress = &ip
	}
	if err := tx.Create(a).Error; err != nil {
		return fmt.Errorf("insert lgpd_audit: %w", err)
	}
	return nil
}
