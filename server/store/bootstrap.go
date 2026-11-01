package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"nvr/server/auth"
	"nvr/server/models"
)

// SettingBootstrapSeeded is the record that the break-glass admin was seeded.
const SettingBootstrapSeeded = "bootstrap_admin_seeded_at"

// Bootstrap runs the boot-time seeding: the three built-in roles, then the
// break-glass admin account. It answers whether an admin account was created.
//
// Roles are seeded on every boot, because an installation must never run with a
// role table an operator cannot assign. `SeedBuiltinRoles` does not rewrite
// roles that exist, so a boot never undoes an operator's permission edits.
//
// The account is the last way in — the one that works when the IdP is down, when
// JIT is disabled, and when an operator has locked everyone out. It is created
// only when BOOTSTRAP_ADMIN_PASSWORD is non-empty and no account holds `admin`;
// an installation that has a way in does not get a second one, and an
// installation with no password set has no local admin at all, which is the
// documented SSO-only shape.
func (s *Store) Bootstrap(user, password string) (bool, error) {
	if err := s.Roles.SeedBuiltinRoles(); err != nil {
		return false, err
	}
	return s.bootstrapAdmin(user, password)
}

// bootstrapAdmin creates the break-glass account, once, and records that it did.
//
// The record is a settings row, not a flag in the account: the account is the
// thing an operator deletes, and a boot that seeds from "no admin exists"
// therefore re-creates the account they deleted. That is the plan's rule
// verbatim — deleting the break-glass admin and restarting must leave the
// installation with no local admin — and it is why the marker outlives the row.
func (s *Store) bootstrapAdmin(user, password string) (bool, error) {
	if password == "" {
		// No password set means no local admin: the SSO-only installation, and
		// the shape a fresh container has before an operator fills .env.
		return false, nil
	}

	seededAt, err := s.Settings.Get(SettingBootstrapSeeded)
	if err == nil && seededAt != "" {
		// Already seeded once. An operator deleted it; that was a decision.
		return false, nil
	} else if err != nil && !isNotFound(err) {
		return false, err
	}

	admins, err := s.Users.CountAdmins()
	if err != nil {
		return false, err
	}
	if admins > 0 {
		// Someone holds admin — an SSO account, an account an operator made.
		// The break-glass account is not seeded, and nothing is recorded, so an
		// installation that later loses every admin can still get one.
		return false, nil
	}

	if existing, err := s.Users.GetByUsername(user); err == nil && existing != nil {
		// A local account already carries this username. Seeding must not take
		// over an account nobody asked it to take over: not its password, not
		// its roles.
		return false, fmt.Errorf("store: cannot seed the break-glass admin: username %q already exists", user)
	} else if !isNotFound(err) {
		return false, err
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return false, err
	}
	created := &models.User{
		ID:        uuid.NewString(),
		Username:  user,
		Provider:  models.ProviderLocal,
		Status:    models.UserStatusActive,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.Users.Create(created); err != nil {
		return false, err
	}
	if err := s.Users.SetPassword(created.ID, hash); err != nil {
		return false, err
	}
	// The admin role only. Not operator, not viewer: this account exists to be
	// used rarely, and what it may do is what an administrator may do.
	if err := s.Users.SetRoles(created.ID, []string{models.RoleAdmin}); err != nil {
		return false, err
	}
	if err := s.Settings.Set(SettingBootstrapSeeded, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return false, err
	}
	return true, nil
}

// isNotFound answers "the row is not there": both lookups this function makes
// answer gorm.ErrRecordNotFound when the row is absent.
func isNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}
