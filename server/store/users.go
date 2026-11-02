package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"nvr/server/models"
)

// UserStore provides persistence for accounts.
type UserStore struct {
	db *gorm.DB
}

// Create inserts an account, minting its id when the caller did not.
func (s *UserStore) Create(u *models.User) error {
	if u.ID == "" {
		u.ID = uuid.NewString()
	}
	if u.Provider == "" {
		u.Provider = models.ProviderLocal
	}
	if err := s.db.Create(u).Error; err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("users: username %q already exists", u.Username)
		}
		return err
	}
	return nil
}

// GetByID fetches an account with its roles loaded. The roles are preloaded
// because every caller that fetches a user is deciding something about their
// access.
func (s *UserStore) GetByID(id string) (*models.User, error) {
	var u models.User
	if err := s.db.Preload("Roles").First(&u, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// GetByUsername fetches an account by login name: the local login path.
func (s *UserStore) GetByUsername(username string) (*models.User, error) {
	var u models.User
	if err := s.db.Preload("Roles").First(&u, "username = ?", username).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// GetByEmail fetches an account by email. The identity contract reads this
// before it links anything, which is why `link_policy` defaults to `pending`.
func (s *UserStore) GetByEmail(email string) (*models.User, error) {
	var u models.User
	if err := s.db.Preload("Roles").First(&u, "email = ?", email).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// FindByProvider fetches the JIT account for a provider's subject: the same
// person signing in twice must land on one account.
//
// Both halves of the pair are required. The local rows carry provider_id = ”
// (the schema's default: a password account has no subject), so an unguarded
// `provider = ? AND provider_id = ?` answers a local account to any assertion
// with no `sub` — the JIT door would open for an IdP that sent no subject.
// An empty half is not a lookup, it is a scan, so it answers not-found. The
// contract gate in sso/ refuses a missing subject too; this is the second line,
// because the store is the thing that hands out accounts.
func (s *UserStore) FindByProvider(provider, providerID string) (*models.User, error) {
	if provider == "" || providerID == "" {
		return nil, gorm.ErrRecordNotFound
	}
	var u models.User
	if err := s.db.Preload("Roles").
		First(&u, "provider = ? AND provider_id = ?", provider, providerID).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// ListAll returns every account, roles included.
func (s *UserStore) ListAll() ([]models.User, error) {
	var users []models.User
	if err := s.db.Preload("Roles").Order("username ASC").Find(&users).Error; err != nil {
		return nil, err
	}
	return users, nil
}

// CountLocalLogin counts the accounts that can authenticate with a password:
// local, active, and carrying a hash.
//
// This is what `GET /api/auth/status` answers `local_login` from. It is a count of
// rows on purpose: the env flag `BOOTSTRAP_ADMIN_PASSWORD` describes a boot, not
// an installation. An operator who deletes the break-glass admin has no password
// way in, and the login page must stop drawing the button — a flag read from the
// environment would keep drawing it.
func (s *UserStore) CountLocalLogin() (int64, error) {
	var count int64
	if err := s.db.Model(&models.User{}).
		Where("provider = ? AND status = ? AND password_hash <> ''", models.ProviderLocal, models.UserStatusActive).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// CountAdmins answers the question the break-glass seeding asks: does anyone
// hold `admin`? An installation that cannot answer this can create an admin it
// did not intend, or discover at 2 a.m. that it has no way in.
func (s *UserStore) CountAdmins() (int64, error) {
	var count int64
	if err := s.db.Model(&models.UserRole{}).
		Joins("JOIN roles ON roles.id = user_roles.role_id").
		Where("roles.name = ?", models.RoleAdmin).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// Update writes the mutable fields, with the same partial-payload rule as the
// camera update: a PUT that omits a column must not zero it. PasswordHash is
// written separately (ChangePassword), so a profile PUT cannot blank a password.
func (s *UserStore) Update(u *models.User) error {
	result := s.db.Model(&models.User{}).Where("id = ?", u.ID).Updates(map[string]any{
		"name":           u.Name,
		"email":          u.Email,
		"username":       u.Username,
		"provider":       u.Provider,
		"provider_id":    u.ProviderID,
		"sso_attributes": u.SSOAttributes,
		"status":         u.Status,
		"updated_at":     time.Now().UTC(),
	})
	if result.Error != nil {
		if isUniqueViolation(result.Error) {
			return fmt.Errorf("users: username %q already exists", u.Username)
		}
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// SetPassword writes an already-hashed password (argon2id, hashed in auth/).
// The store takes the hash, never the password: there is one place that knows
// how a password becomes a hash.
func (s *UserStore) SetPassword(id, passwordHash string) error {
	result := s.db.Model(&models.User{}).Where("id = ?", id).Updates(map[string]any{
		"password_hash": passwordHash,
		"updated_at":    time.Now().UTC(),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// MarkLogin records the last successful login.
func (s *UserStore) MarkLogin(id string, at time.Time) error {
	return s.db.Model(&models.User{}).Where("id = ?", id).Update("last_login", at).Error
}

// Count reports how many accounts exist.
func (s *UserStore) Count() (int64, error) {
	var count int64
	if err := s.db.Model(&models.User{}).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// Delete removes an account, its role grants and its sessions in one
// transaction: an account deleted with live sessions leaves tokens that verify
// against a person who no longer exists.
func (s *UserStore) Delete(id string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		res := tx.Delete(&models.User{}, "id = ?", id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		if err := tx.Where("user_id = ?", id).Delete(&models.UserRole{}).Error; err != nil {
			return err
		}
		return tx.Where("user_id = ?", id).Delete(&models.Session{}).Error
	})
}

// RolesFor returns an account's role names, the list the JWT claims carry and
// the role middleware compares against.
func (s *UserStore) RolesFor(id string) ([]string, error) {
	var names []string
	if err := s.db.Model(&models.UserRole{}).
		Joins("JOIN roles ON roles.id = user_roles.role_id").
		Where("user_roles.user_id = ?", id).
		Order("roles.name ASC").Pluck("roles.name", &names).Error; err != nil {
		return nil, err
	}
	return names, nil
}

// SetRoles replaces an account's roles with the named set, in one transaction.
// Replace, not append: "give this person operator" that leaves admin attached
// is a role change nobody made.
func (s *UserStore) SetRoles(id string, roleNames []string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var user models.User
		if err := tx.First(&user, "id = ?", id).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", id).Delete(&models.UserRole{}).Error; err != nil {
			return err
		}
		for _, name := range roleNames {
			var role models.Role
			if err := tx.Where("name = ?", name).First(&role).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return fmt.Errorf("users: role %q does not exist", name)
				}
				return err
			}
			if err := tx.Create(&models.UserRole{
				ID:        uuid.NewString(),
				UserID:    id,
				RoleID:    role.ID,
				CreatedAt: time.Now().UTC(),
			}).Error; err != nil {
				return err
			}
		}
		return tx.Model(&models.User{}).Where("id = ?", id).Update("updated_at", time.Now().UTC()).Error
	})
}

// UserHasRole reports whether an account holds a role. The role middleware and
// the break-glass seeding both ask.
func (s *UserStore) UserHasRole(id, roleName string) (bool, error) {
	var count int64
	if err := s.db.Model(&models.UserRole{}).
		Joins("JOIN roles ON roles.id = user_roles.role_id").
		Where("user_roles.user_id = ? AND roles.name = ?", id, roleName).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}
