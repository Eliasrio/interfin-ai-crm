// emma_files.go — репозиторий библиотеки файлов для отправки (EP-04,
// таблица 0018). Единственная точка доступа к emma_send_files: и ручки
// панели, и валидация маркеров воркера ходят через этот интерфейс.
package repo

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

// NewEmmaSendFiles — репозиторий emma_send_files поверх того же *gorm.DB.
func NewEmmaSendFiles(db *gorm.DB) EmmaSendFilesRepo {
	return &emmaSendFilesRepo{db: db}
}

type emmaSendFilesRepo struct{ db *gorm.DB }

func (r *emmaSendFilesRepo) List(ctx context.Context) ([]models.EmmaSendFile, error) {
	var files []models.EmmaSendFile
	// id DESC вторым ключом — детерминированный порядок при равных created_at
	// (та же дисциплина, что emma_kb_files).
	err := r.db.WithContext(ctx).
		Order("created_at DESC, id DESC").
		Find(&files).Error
	if err != nil {
		return nil, fmt.Errorf("repo: list send files: %w", err)
	}
	return files, nil
}

func (r *emmaSendFilesRepo) GetByID(ctx context.Context, id int64) (*models.EmmaSendFile, error) {
	var f models.EmmaSendFile
	if err := r.db.WithContext(ctx).First(&f, id).Error; err != nil {
		return nil, wrapNotFound(err, "repo: get send file")
	}
	return &f, nil
}

func (r *emmaSendFilesRepo) Create(ctx context.Context, f *models.EmmaSendFile) error {
	if err := r.db.WithContext(ctx).Create(f).Error; err != nil {
		return fmt.Errorf("repo: create send file %q: %w", f.Name, err)
	}
	// CreatedAt заполняет DEFAULT now() БД — перечитываем, чтобы ручка отдала
	// реальное время (дисциплина CreateVersion EP-02).
	if err := r.db.WithContext(ctx).First(f, f.ID).Error; err != nil {
		return fmt.Errorf("repo: create send file: reload: %w", err)
	}
	return nil
}

func (r *emmaSendFilesRepo) Update(ctx context.Context, id int64, upd EmmaSendFileUpdate) (*models.EmmaSendFile, error) {
	fields := map[string]interface{}{}
	if upd.Name != nil {
		fields["name"] = *upd.Name
	}
	if upd.Description != nil {
		fields["description"] = *upd.Description
	}
	if upd.IsActive != nil {
		fields["is_active"] = *upd.IsActive
	}
	if len(fields) > 0 {
		res := r.db.WithContext(ctx).Model(&models.EmmaSendFile{}).
			Where("id = ?", id).
			Updates(fields)
		if res.Error != nil {
			return nil, fmt.Errorf("repo: update send file %d: %w", id, res.Error)
		}
		if res.RowsAffected == 0 {
			return nil, fmt.Errorf("repo: update send file %d: %w", id, ErrNotFound)
		}
	}
	var f models.EmmaSendFile
	if err := r.db.WithContext(ctx).First(&f, id).Error; err != nil {
		return nil, wrapNotFound(err, fmt.Sprintf("repo: update send file %d: reload", id))
	}
	return &f, nil
}

func (r *emmaSendFilesRepo) Delete(ctx context.Context, id int64) (*models.EmmaSendFile, error) {
	var f models.EmmaSendFile
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&f, id).Error; err != nil {
			return err
		}
		// emma_events.send_file_id обнуляет FK ON DELETE SET NULL (0020) —
		// журнал статистики EP-06 переживает удаление файла.
		if err := tx.Delete(&models.EmmaSendFile{}, id).Error; err != nil {
			return fmt.Errorf("delete row: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, wrapNotFound(err, fmt.Sprintf("repo: delete send file %d", id))
	}
	return &f, nil
}

func (r *emmaSendFilesRepo) ListActive(ctx context.Context) ([]models.EmmaSendFile, error) {
	var files []models.EmmaSendFile
	// Старые первыми: у секции промпта стабильный порядок — кэш 30 с не
	// «перетасовывает» файлы между пересборками.
	err := r.db.WithContext(ctx).
		Where("is_active").
		Order("id ASC").
		Find(&files).Error
	if err != nil {
		return nil, fmt.Errorf("repo: list active send files: %w", err)
	}
	return files, nil
}
