package repo

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

// ManagerRepo — managers (M7, §5.2). Выборки видят только active=TRUE:
// деактивированный менеджер не логинится и не рефрешится — единообразный
// 401 без утечки причины.
type ManagerRepo interface {
	// Create вставляет менеджера; заполняет m.ID. PasswordHash обязан быть
	// bcrypt-хешем (auth.HashPassword), не паролем.
	Create(ctx context.Context, m *models.Manager) error
	// GetByEmail — выборка логина. ErrNotFound, если нет или неактивен.
	GetByEmail(ctx context.Context, email string) (*models.Manager, error)
	// GetByID — выборка при refresh. ErrNotFound, если нет или неактивен.
	GetByID(ctx context.Context, id int64) (*models.Manager, error)
}

// RefreshTokenRepo — refresh_tokens (M7, §5.1). Хранит только хеши
// (auth.HashRefreshToken), сами токены сервер не видит после выдачи.
type RefreshTokenRepo interface {
	// Create сохраняет хеш выданного токена.
	Create(ctx context.Context, rt *models.RefreshToken) error
	// Consume атомарно удаляет токен по хешу и возвращает строку (ротация:
	// каждый refresh гасит предъявленный токен, даже просроченный —
	// проверка expires_at за вызывающим). ErrNotFound — токена нет или он
	// уже использован; конкурентный double-spend невозможен:
	// DELETE ... RETURNING достаётся ровно одному запросу.
	Consume(ctx context.Context, tokenHash string) (*models.RefreshToken, error)
	// DeleteExpired чистит просроченные строки (best-effort гигиена:
	// зовётся при логине, отдельного cron в M7 нет).
	DeleteExpired(ctx context.Context) error
}

// NewAuth создаёт репозитории M7 поверх того же *gorm.DB (SimpleProtocol §4.2).
func NewAuth(db *gorm.DB) (ManagerRepo, RefreshTokenRepo) {
	return &managerRepo{db: db}, &refreshTokenRepo{db: db}
}

// --- managers ---

type managerRepo struct{ db *gorm.DB }

func (r *managerRepo) Create(ctx context.Context, m *models.Manager) error {
	if err := r.db.WithContext(ctx).Create(m).Error; err != nil {
		return fmt.Errorf("repo: create manager: %w", err)
	}
	return nil
}

func (r *managerRepo) GetByEmail(ctx context.Context, email string) (*models.Manager, error) {
	var m models.Manager
	err := r.db.WithContext(ctx).
		Where("email = ? AND active", email).
		First(&m).Error
	if err != nil {
		return nil, wrapNotFound(err, "repo: get manager by email")
	}
	return &m, nil
}

func (r *managerRepo) GetByID(ctx context.Context, id int64) (*models.Manager, error) {
	var m models.Manager
	err := r.db.WithContext(ctx).
		Where("id = ? AND active", id).
		First(&m).Error
	if err != nil {
		return nil, wrapNotFound(err, "repo: get manager by id")
	}
	return &m, nil
}

// --- refresh_tokens ---

type refreshTokenRepo struct{ db *gorm.DB }

func (r *refreshTokenRepo) Create(ctx context.Context, rt *models.RefreshToken) error {
	if err := r.db.WithContext(ctx).Create(rt).Error; err != nil {
		return fmt.Errorf("repo: create refresh token: %w", err)
	}
	return nil
}

func (r *refreshTokenRepo) Consume(ctx context.Context, tokenHash string) (*models.RefreshToken, error) {
	var rt models.RefreshToken
	// DELETE ... RETURNING: выборка и гашение — одна команда, окна между
	// «проверили» и «удалили» нет.
	res := r.db.WithContext(ctx).
		Clauses(clause.Returning{}).
		Where("token_hash = ?", tokenHash).
		Delete(&rt)
	if res.Error != nil {
		return nil, fmt.Errorf("repo: consume refresh token: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return nil, fmt.Errorf("repo: consume refresh token: %w", ErrNotFound)
	}
	return &rt, nil
}

func (r *refreshTokenRepo) DeleteExpired(ctx context.Context) error {
	err := r.db.WithContext(ctx).
		Where("expires_at < ?", time.Now()).
		Delete(&models.RefreshToken{}).Error
	if err != nil {
		return fmt.Errorf("repo: delete expired refresh tokens: %w", err)
	}
	return nil
}
