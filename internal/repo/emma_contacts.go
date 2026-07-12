// emma_contacts.go — репозиторий справочника контактов Эммы (EP-05,
// таблица 0019). Единственная точка доступа к emma_contacts: ручки панели
// и секция system-блока ходят через этот интерфейс.
//
// Лимит 30 активных (ТЗ §3, бюджет system-блока) охраняется ЗДЕСЬ, в
// транзакции с advisory-локом: две конкурентные записи (Create/Update),
// включающие is_active, сериализуются — 31-й активный невозможен и в гонке
// (критерий приёмки EP-05).
package repo

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/interfin/interfin-ai-crm/internal/models"
)

// MaxActiveContacts — потолок активных контактов (ТЗ §3): секция промпта
// делит бюджет system-блока 5000 с базой знаний.
const MaxActiveContacts = 30

// ErrContactsLimit — попытка включить 31-й активный контакт; ручка мапит
// в 400 {"code":"CONTACTS_LIMIT","limit":30}.
var ErrContactsLimit = errors.New("repo: лимит активных контактов исчерпан")

// contactsLockID — ключ pg_advisory_xact_lock для проверок лимита.
// Захватывается только транзакциями, включающими is_active; лок живёт до
// конца транзакции (xact) — явного release не нужно.
const contactsLockID = 0xE05_C047AC75 // EP-05 contacts

func NewEmmaContacts(db *gorm.DB) EmmaContactsRepo {
	return &emmaContactsRepo{db: db}
}

type emmaContactsRepo struct{ db *gorm.DB }

func (r *emmaContactsRepo) List(ctx context.Context) ([]models.EmmaContact, error) {
	var contacts []models.EmmaContact
	// id ASC вторым ключом: новые при равном sort_order — в конец
	// (task EP-05 §1), порядок детерминирован.
	err := r.db.WithContext(ctx).
		Order("sort_order ASC, id ASC").
		Find(&contacts).Error
	if err != nil {
		return nil, fmt.Errorf("repo: list contacts: %w", err)
	}
	return contacts, nil
}

func (r *emmaContactsRepo) GetByID(ctx context.Context, id int64) (*models.EmmaContact, error) {
	var c models.EmmaContact
	if err := r.db.WithContext(ctx).First(&c, id).Error; err != nil {
		return nil, wrapNotFound(err, "repo: get contact")
	}
	return &c, nil
}

func (r *emmaContactsRepo) Create(ctx context.Context, c *models.EmmaContact) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if c.IsActive {
			if err := guardActiveLimit(tx, 0); err != nil {
				return err
			}
		}
		return tx.Create(c).Error
	})
	if err != nil {
		if errors.Is(err, ErrContactsLimit) {
			return ErrContactsLimit
		}
		return fmt.Errorf("repo: create contact %q: %w", c.Name, err)
	}
	return nil
}

func (r *emmaContactsRepo) Update(ctx context.Context, id int64, upd EmmaContactUpdate) (*models.EmmaContact, error) {
	fields := map[string]interface{}{}
	if upd.Type != nil {
		fields["type"] = *upd.Type
	}
	if upd.Name != nil {
		fields["name"] = *upd.Name
	}
	if upd.Value != nil {
		fields["value"] = *upd.Value
	}
	if upd.Comment != nil {
		if *upd.Comment == "" {
			fields["comment"] = nil // пустой комментарий = NULL (колонка nullable)
		} else {
			fields["comment"] = *upd.Comment
		}
	}
	if upd.IsActive != nil {
		fields["is_active"] = *upd.IsActive
	}
	if upd.SortOrder != nil {
		fields["sort_order"] = *upd.SortOrder
	}

	var c models.EmmaContact
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Лимит проверяется только когда PATCH ВКЛЮЧАЕТ контакт: остальные
		// правки (в т.ч. выключение) лок не берут — не сериализуются зря.
		if upd.IsActive != nil && *upd.IsActive {
			if err := guardActiveLimit(tx, id); err != nil {
				return err
			}
		}
		if len(fields) > 0 {
			res := tx.Model(&models.EmmaContact{}).Where("id = ?", id).Updates(fields)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return ErrNotFound
			}
		}
		return tx.First(&c, id).Error
	})
	if err != nil {
		if errors.Is(err, ErrContactsLimit) {
			return nil, ErrContactsLimit
		}
		return nil, wrapNotFound(err, fmt.Sprintf("repo: update contact %d", id))
	}
	return &c, nil
}

func (r *emmaContactsRepo) Delete(ctx context.Context, id int64) error {
	res := r.db.WithContext(ctx).Delete(&models.EmmaContact{}, id)
	if res.Error != nil {
		return fmt.Errorf("repo: delete contact %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("repo: delete contact %d: %w", id, ErrNotFound)
	}
	return nil
}

func (r *emmaContactsRepo) ListActive(ctx context.Context) ([]models.EmmaContact, error) {
	var contacts []models.EmmaContact
	err := r.db.WithContext(ctx).
		Where("is_active").
		Order("sort_order ASC, id ASC").
		Find(&contacts).Error
	if err != nil {
		return nil, fmt.Errorf("repo: list active contacts: %w", err)
	}
	return contacts, nil
}

// guardActiveLimit сериализует включающие транзакции advisory-локом и
// проверяет, что активных контактов КРОМЕ excludeID меньше лимита.
// Гонка двух PATCH: второй ждёт лок до коммита первого и видит его строку —
// 31-й активный получает ErrContactsLimit, а не проскакивает (ТЗ §3).
func guardActiveLimit(tx *gorm.DB, excludeID int64) error {
	if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", int64(contactsLockID)).Error; err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	var n int64
	if err := tx.Model(&models.EmmaContact{}).
		Where("is_active AND id <> ?", excludeID).
		Count(&n).Error; err != nil {
		return fmt.Errorf("count active: %w", err)
	}
	if n >= MaxActiveContacts {
		return ErrContactsLimit
	}
	return nil
}
