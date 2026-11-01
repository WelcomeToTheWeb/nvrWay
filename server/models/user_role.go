package models

import "time"

// UserRole is the junction row linking an account to a role. It is a row and
// not only a join, so "who gave this person admin, when" has a timestamp.
type UserRole struct {
	ID        string    `gorm:"type:uuid;primaryKey" json:"id"`
	UserID    string    `gorm:"type:uuid;index;not null" json:"user_id"`
	RoleID    string    `gorm:"type:uuid;index;not null" json:"role_id"`
	CreatedAt time.Time `json:"created_at"`
}

// TableName pins the table name.
func (UserRole) TableName() string { return "user_roles" }
