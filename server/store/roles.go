package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"nvr/server/models"
)

// RoleStore provides CRUD for roles.
type RoleStore struct {
	db *gorm.DB
}

// ErrRoleNotFound is the named role lookup failure, so a handler answers 404
// without matching an error string.
var ErrRoleNotFound = errors.New("role not found")

// CreateRole creates a role.
func (s *RoleStore) CreateRole(role *models.Role) error {
	if role.ID == "" {
		role.ID = uuid.NewString()
	}
	if err := s.db.Create(role).Error; err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("roles: name %q already exists", role.Name)
		}
		return err
	}
	return nil
}

// GetRoleByID retrieves a role by id.
func (s *RoleStore) GetRoleByID(id string) (*models.Role, error) {
	var role models.Role
	if err := s.db.First(&role, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrRoleNotFound
		}
		return nil, err
	}
	return &role, nil
}

// GetRoleByName retrieves a role by name.
func (s *RoleStore) GetRoleByName(name string) (*models.Role, error) {
	var role models.Role
	if err := s.db.First(&role, "name = ?", name).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrRoleNotFound
		}
		return nil, err
	}
	return &role, nil
}

// ListRoles returns every role.
func (s *RoleStore) ListRoles() ([]models.Role, error) {
	var roles []models.Role
	if err := s.db.Order("name ASC").Find(&roles).Error; err != nil {
		return nil, err
	}
	return roles, nil
}

// UpdateRole writes a role's description and permissions.
//
// The write is struct-based, not a map: `permissions` is a `serializer:json`
// column, and the serializer runs on the struct path. A map write hands the
// driver a []string, which is a runtime error on sqlite ("row value misused")
// and a corrupted column on others.
func (s *RoleStore) UpdateRole(role *models.Role) error {
	result := s.db.Model(&models.Role{}).Where("id = ?", role.ID).Updates(models.Role{
		Description: role.Description,
		Permissions: role.Permissions,
		UpdatedAt:   time.Now().UTC(),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrRoleNotFound
	}
	return nil
}

// DeleteRole removes a role and the grants pointing at it. Built-in roles are
// not deletable: deleting `viewer` deletes the JIT floor, and every account
// mapped to it silently loses its access.
func (s *RoleStore) DeleteRole(id string) error {
	var role models.Role
	if err := s.db.First(&role, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrRoleNotFound
		}
		return err
	}
	if role.IsBuiltin {
		return fmt.Errorf("roles: %q is a built-in role and cannot be deleted", role.Name)
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("role_id = ?", id).Delete(&models.UserRole{}).Error; err != nil {
			return err
		}
		return tx.Delete(&models.Role{}, "id = ?", id).Error
	})
}

// SeedBuiltinRoles creates admin, operator and viewer when they do not exist.
//
// It is called from store construction, so an installation never runs with a
// role table an operator cannot assign. It does not rewrite existing roles: an
// installation that renamed a role's permissions has chosen them, and a boot
// that resets them is a boot that undoes an operator.
func (s *RoleStore) SeedBuiltinRoles() error {
	for _, builtin := range models.DefaultBuiltinRoles() {
		_, err := s.GetRoleByName(builtin.Name)
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrRoleNotFound) {
			return err
		}
		role := &models.Role{
			ID:          uuid.NewString(),
			Name:        builtin.Name,
			Description: builtin.Description,
			Permissions: builtin.Permissions,
			IsBuiltin:   true,
		}
		if err := s.db.Create(role).Error; err != nil {
			return fmt.Errorf("failed to seed role %s: %w", builtin.Name, err)
		}
	}
	return nil
}

// UserRoleStore provides persistence for the account→role grants.
type UserRoleStore struct {
	db *gorm.DB
}

// ListFor returns an account's grants.
func (s *UserRoleStore) ListFor(userID string) ([]models.UserRole, error) {
	var grants []models.UserRole
	if err := s.db.Where("user_id = ?", userID).Order("created_at ASC").Find(&grants).Error; err != nil {
		return nil, err
	}
	return grants, nil
}

// Grant links an account to a role.
func (s *UserRoleStore) Grant(userID, roleID string) error {
	var existing int64
	if err := s.db.Model(&models.UserRole{}).
		Where("user_id = ? AND role_id = ?", userID, roleID).Count(&existing).Error; err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}
	return s.db.Create(&models.UserRole{
		ID:        uuid.NewString(),
		UserID:    userID,
		RoleID:    roleID,
		CreatedAt: time.Now().UTC(),
	}).Error
}

// Revoke unlinks an account from a role.
func (s *UserRoleStore) Revoke(userID, roleID string) error {
	result := s.db.Where("user_id = ? AND role_id = ?", userID, roleID).
		Delete(&models.UserRole{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}
