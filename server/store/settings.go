package store

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"nvr/server/models"
)

// SettingStore provides persistence for the installation facts the server owns.
type SettingStore struct {
	db *gorm.DB
}

// Set writes one key. The write is an upsert, because these keys are written
// from ticks and restarts, and a first-write-only path would silently drop the
// second.
func (s *SettingStore) Set(key, value string) error {
	if key == "" {
		return errors.New("settings: key is required")
	}
	now := time.Now().UTC()
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.Assignments(map[string]any{"value": value, "updated_at": now}),
	}).Create(&models.Setting{Key: key, Value: value}).Error
}

// Get reads one key, answering gorm.ErrRecordNotFound when it is absent.
func (s *SettingStore) Get(key string) (string, error) {
	var setting models.Setting
	if err := s.db.First(&setting, "key = ?", key).Error; err != nil {
		return "", err
	}
	return setting.Value, nil
}

// GetOr answers the value, or `fallback` when the key is absent. A read failure
// other than "absent" is not folded into the fallback: a fallback returned for
// a broken database is a policy nobody chose.
func (s *SettingStore) GetOr(key, fallback string) (string, error) {
	value, err := s.Get(key)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fallback, nil
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

// GetInt reads a numeric setting (the recorder's generation counter). An
// unparseable value is an error, not zero: a recorder that reads a broken
// counter as 0 starts numbering over and overwrites files.
func (s *SettingStore) GetInt(key string) (int, error) {
	value, err := s.Get(key)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("settings: %s is %q, not a number", key, value)
	}
	return n, nil
}

// ListKeys returns every key under a prefix, for the diagnostics that name the
// state the installation is carrying (recorder generations, pinned host keys).
func (s *SettingStore) ListKeys(prefix string) ([]models.Setting, error) {
	var settings []models.Setting
	if err := s.db.Where("key LIKE ?", prefix+"%").Order("key ASC").Find(&settings).Error; err != nil {
		return nil, err
	}
	return settings, nil
}

// Delete removes a key.
func (s *SettingStore) Delete(key string) error {
	result := s.db.Delete(&models.Setting{}, "key = ?", key)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}
