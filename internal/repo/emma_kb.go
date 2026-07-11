// emma_kb.go — репозиторий файлов базы знаний панели Эммы (EP-03,
// таблица 0017). Единственная точка доступа к emma_kb_files: upsert по
// filename и транзакционное удаление «строка + чанки» живут здесь,
// а не в хендлерах.
package repo

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

// NewEmmaKB — репозиторий emma_kb_files поверх того же *gorm.DB.
func NewEmmaKB(db *gorm.DB) EmmaKBRepo {
	return &emmaKBRepo{db: db}
}

type emmaKBRepo struct{ db *gorm.DB }

func (r *emmaKBRepo) List(ctx context.Context) ([]models.EmmaKBFile, error) {
	var files []models.EmmaKBFile
	// id DESC вторым ключом — детерминированный порядок при равных created_at.
	err := r.db.WithContext(ctx).
		Order("created_at DESC, id DESC").
		Find(&files).Error
	if err != nil {
		return nil, fmt.Errorf("repo: list kb files: %w", err)
	}
	return files, nil
}

func (r *emmaKBRepo) GetByID(ctx context.Context, id int64) (*models.EmmaKBFile, error) {
	var f models.EmmaKBFile
	if err := r.db.WithContext(ctx).First(&f, id).Error; err != nil {
		return nil, wrapNotFound(err, "repo: get kb file")
	}
	return &f, nil
}

func (r *emmaKBRepo) UpsertByFilename(ctx context.Context, f *models.EmmaKBFile) (string, error) {
	var row struct {
		ID        int64     `gorm:"column:id"`
		CreatedAt time.Time `gorm:"column:created_at"`
		OldPath   *string   `gorm:"column:old_path"`
	}
	// CTE old читается из снапшота ДО вставки: RETURNING видит только новые
	// значения, а вытесненный file_path нужен вызывающему, чтобы удалить
	// заменённый оригинал с диска.
	err := r.db.WithContext(ctx).Raw(`
		WITH old AS (
			SELECT file_path FROM emma_kb_files WHERE filename = ?
		)
		INSERT INTO emma_kb_files
			(filename, mime_type, file_path, file_size,
			 index_status, index_error, chunks_count)
		VALUES (?, ?, ?, ?, 'pending', NULL, 0)
		ON CONFLICT (filename) DO UPDATE SET
			mime_type    = EXCLUDED.mime_type,
			file_path    = EXCLUDED.file_path,
			file_size    = EXCLUDED.file_size,
			index_status = 'pending',
			index_error  = NULL,
			chunks_count = 0
		RETURNING id, created_at, (SELECT file_path FROM old) AS old_path`,
		f.Filename, f.Filename, f.MimeType, f.FilePath, f.FileSize,
	).Scan(&row).Error
	if err != nil {
		return "", fmt.Errorf("repo: upsert kb file %q: %w", f.Filename, err)
	}

	f.ID = row.ID
	f.CreatedAt = row.CreatedAt
	f.IndexStatus = models.EmmaKBPending
	f.IndexError = nil
	f.ChunksCount = 0

	oldPath := ""
	if row.OldPath != nil {
		oldPath = *row.OldPath
	}
	return oldPath, nil
}

func (r *emmaKBRepo) SetStatus(ctx context.Context, id int64, status string, chunks int, errText *string) error {
	res := r.db.WithContext(ctx).Model(&models.EmmaKBFile{}).
		Where("id = ?", id).
		Updates(map[string]interface{}{
			"index_status": status,
			"chunks_count": chunks,
			"index_error":  errText,
		})
	if res.Error != nil {
		return fmt.Errorf("repo: set kb status %d → %s: %w", id, status, res.Error)
	}
	if res.RowsAffected == 0 {
		// Файл удалили, пока задача индексации стояла в очереди.
		return ErrNotFound
	}
	return nil
}

func (r *emmaKBRepo) Delete(ctx context.Context, id int64) (*models.EmmaKBFile, error) {
	var f models.EmmaKBFile
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&f, id).Error; err != nil {
			return err
		}
		// Та же операция, что ReplaceSource(source, nil) в rag.go, но внутри
		// ЭТОЙ транзакции: отдельный вызов ReplaceSource открыл бы вторую
		// транзакцию, и атомарность «чанки + строка» развалилась бы.
		if err := tx.Where("source = ?", models.EmmaKBSource(id)).
			Delete(&models.KnowledgeChunk{}).Error; err != nil {
			return fmt.Errorf("delete chunks: %w", err)
		}
		if err := tx.Delete(&models.EmmaKBFile{}, id).Error; err != nil {
			return fmt.Errorf("delete row: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, wrapNotFound(err, fmt.Sprintf("repo: delete kb file %d", id))
	}
	return &f, nil
}
