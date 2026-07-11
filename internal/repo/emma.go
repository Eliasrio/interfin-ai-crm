// emma.go — репозиторий версий системного промпта Эммы (EP-02, таблица 0016).
package repo

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

// NewEmmaPrompts — репозиторий emma_prompt_versions поверх того же *gorm.DB.
func NewEmmaPrompts(db *gorm.DB) EmmaPromptRepo {
	return &emmaPromptRepo{db: db}
}

type emmaPromptRepo struct{ db *gorm.DB }

func (r *emmaPromptRepo) GetCurrent(ctx context.Context) (*models.EmmaPromptVersion, error) {
	var v models.EmmaPromptVersion
	// Выборка ложится на частичный уникальный индекс emma_prompt_current_key.
	if err := r.db.WithContext(ctx).First(&v, "is_current").Error; err != nil {
		return nil, wrapNotFound(err, "repo: get current prompt")
	}
	return &v, nil
}

func (r *emmaPromptRepo) CreateVersion(ctx context.Context, v *models.EmmaPromptVersion) error {
	v.ID = 0 // restore передаёт копию старой версии — PK обязан выдать BIGSERIAL
	v.IsCurrent = true
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Снятие флага и вставка — атомарны. При гонке двух CreateVersion
		// проигравший не видит строку победителя в своём снапшоте UPDATE
		// и падает на уникальном индексе 0016 при INSERT — транзакция
		// откатывается целиком, две активные версии невозможны.
		if err := tx.Model(&models.EmmaPromptVersion{}).
			Where("is_current").
			Update("is_current", false).Error; err != nil {
			return fmt.Errorf("clear current: %w", err)
		}
		if err := tx.Create(v).Error; err != nil {
			return fmt.Errorf("insert version: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("repo: create prompt version: %w", err)
	}
	// CreatedAt заполняет DEFAULT now() БД — перечитываем строку, чтобы
	// хендлер отдал реальное время, а не нулевое поле структуры.
	if err := r.db.WithContext(ctx).First(v, v.ID).Error; err != nil {
		return fmt.Errorf("repo: create prompt version: reload: %w", err)
	}
	return nil
}

func (r *emmaPromptRepo) History(ctx context.Context, page, perPage int) ([]EmmaPromptHistoryItem, int64, error) {
	if page < 1 || perPage <= 0 {
		return nil, 0, fmt.Errorf("repo: prompt history: page %d / perPage %d невалидны", page, perPage)
	}
	q := r.db.WithContext(ctx).Model(&models.EmmaPromptVersion{})
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("repo: prompt history: count: %w", err)
	}
	var items []EmmaPromptHistoryItem
	// LEFT() считает СИМВОЛЫ (руны), не байты — кириллица в превью не рвётся;
	// полный текст версии по сети не гоняется (история не ограничена).
	// id DESC вторым ключом — детерминированный порядок при равных created_at.
	err := r.db.WithContext(ctx).Model(&models.EmmaPromptVersion{}).
		Select("id, created_at, created_by, LEFT(system_prompt, 100) AS preview, style").
		Order("created_at DESC, id DESC").
		Limit(perPage).Offset((page - 1) * perPage).
		Find(&items).Error
	if err != nil {
		return nil, 0, fmt.Errorf("repo: prompt history: %w", err)
	}
	return items, total, nil
}

func (r *emmaPromptRepo) GetByID(ctx context.Context, id int64) (*models.EmmaPromptVersion, error) {
	var v models.EmmaPromptVersion
	if err := r.db.WithContext(ctx).First(&v, id).Error; err != nil {
		return nil, wrapNotFound(err, "repo: get prompt version")
	}
	return &v, nil
}
