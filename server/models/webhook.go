package models

import "time"

// Delivery statuses. Each attempt is a row, so "the endpoint returned 502
// three times then 200" is readable without reconstructing it.
const (
	DeliveryStatusPending   = "pending"
	DeliveryStatusDelivered = "delivered"
	DeliveryStatusFailed    = "failed"
)

// Webhook is one outbound notification endpoint.
//
// v1 delivers event closes: the payload names the window, the classes, zones
// and severities, and the URLs the server answers for snapshot and clip. Each
// delivery is signed `X-NVR-Signature: sha256=<hex(HMAC-SHA256(secret, body))>`
// so a receiver can prove it came from this installation and not from someone
// who found the URL.
type Webhook struct {
	ID   string `gorm:"type:uuid;primaryKey" json:"id"`
	Name string `gorm:"uniqueIndex;not null" json:"name"`
	URL  string `gorm:"not null" json:"url"`

	// Secret is the HMAC signing key. `json:"-"`: the console writes it and
	// never reads it back, exactly like a storage backend's credentials.
	Secret string `gorm:"type:text;not null;default:''" json:"-"`

	// CameraID empty is every camera.
	CameraID string `gorm:"not null;default:'';index" json:"camera_id"`

	// Events is the JSON array of triggers this endpoint wants
	// (v1: "event.close"). Stored as text so the ledger stays one table when
	// more triggers arrive.
	Events  string `gorm:"type:text;not null;default:'[\"event.close\"]'" json:"events"`
	Enabled bool   `gorm:"not null;default:true" json:"enabled"`

	LastError       string     `gorm:"type:text;not null;default:''" json:"last_error"`
	LastDeliveredAt *time.Time `json:"last_delivered_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName pins the table name.
func (Webhook) TableName() string { return "webhooks" }

// SecretSet is what the console may know about the signing secret: that it is
// set. The value has no path out of the API.
func (w *Webhook) SecretSet() bool { return w != nil && w.Secret != "" }

// Delivers reports whether this endpoint subscribes to the given trigger.
func (w *Webhook) Delivers(trigger string) bool {
	for _, e := range JSONStringList(w.Events) {
		if e == trigger {
			return true
		}
	}
	return false
}

// WebhookDelivery is one delivery attempt and what came back.
type WebhookDelivery struct {
	ID        string `gorm:"type:uuid;primaryKey" json:"id"`
	WebhookID string `gorm:"type:uuid;not null;index:idx_deliveries_webhook_created" json:"webhook_id"`
	Event     string `gorm:"not null" json:"event"`
	Payload   string `gorm:"type:text;not null" json:"payload"`

	Status     string `gorm:"not null;default:'pending';index:idx_deliveries_webhook_created" json:"status"`
	StatusCode int    `json:"status_code"`
	Attempt    int    `gorm:"not null;default:1" json:"attempt"`
	Error      string `gorm:"type:text;not null;default:''" json:"error"`

	DeliveredAt *time.Time `json:"delivered_at"`
	CreatedAt   time.Time  `gorm:"index:idx_deliveries_webhook_created" json:"created_at"`
}

// TableName pins the table name.
func (WebhookDelivery) TableName() string { return "webhook_deliveries" }
