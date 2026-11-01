package models

import "time"

// Storage backend kinds (plan decision 4). `fs` is a local directory, which
// includes an NFS/SMB mount and a compose volume — the driver is the same, and
// the deployment answers "where does footage go" with a path.
const (
	StorageKindS3     = "s3"
	StorageKindWebDAV = "webdav"
	StorageKindSFTP   = "sftp"
	StorageKindFS     = "fs"
)

// StorageKinds is every backend kind.
var StorageKinds = []string{StorageKindS3, StorageKindWebDAV, StorageKindSFTP, StorageKindFS}

// ValidStorageKind reports whether kind is a storage backend kind.
func ValidStorageKind(kind string) bool {
	for _, k := range StorageKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// StorageBackend is one destination for recorded media.
//
// The four secret columns hold **sealed** text (AES-256-GCM, base64, see
// `storage/seal.go`), never plaintext, and every one of them is `json:"-"`: no
// endpoint echoes a secret, the console sees `secret_set: true`. A database
// backup is therefore readable and a credential leak needs the seal key, which
// never leaves the process.
type StorageBackend struct {
	ID   string `gorm:"type:uuid;primaryKey" json:"id"`
	Name string `gorm:"uniqueIndex;not null" json:"name"`
	Kind string `gorm:"not null;index" json:"kind"`

	// S3-shaped backends.
	Endpoint string `gorm:"not null;default:''" json:"endpoint"`
	Region   string `gorm:"not null;default:''" json:"region"`
	Bucket   string `gorm:"not null;default:''" json:"bucket"`
	Prefix   string `gorm:"not null;default:''" json:"prefix"`

	// WebDAV / SFTP credentials. Password and PrivateKeyPath are sealed.
	Username string `gorm:"not null;default:''" json:"username"`
	Password string `gorm:"type:text;not null;default:''" json:"-"`

	// S3 credentials, sealed.
	AccessKey string `gorm:"not null;default:''" json:"-"`
	SecretKey string `gorm:"type:text;not null;default:''" json:"-"`

	// SFTP: the private key file **on the server host**, sealed as a path
	// (which key file is used is information).
	PrivateKeyPath string `gorm:"type:text;not null;default:''" json:"-"`

	// InsecureTLS flips verification off for self-hosted MinIO with a
	// self-signed certificate. The name is the deployment's, not a driver's.
	InsecureTLS bool `gorm:"not null;default:false" json:"insecure_tls"`

	// fs: the root directory (a local disk, an NFS mount, a compose volume).
	RootPath string `gorm:"not null;default:''" json:"root_path"`

	Enabled   bool `gorm:"not null;default:true" json:"enabled"`
	IsDefault bool `gorm:"not null;default:false" json:"is_default"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName pins the table name.
func (StorageBackend) TableName() string { return "storage_backends" }

// SecretSet renders what the console is allowed to know about a secret column:
// that it is set. The value itself has no path out of the API.
func (b *StorageBackend) SecretSet() bool {
	if b == nil {
		return false
	}
	return b.Password != "" || b.AccessKey != "" || b.SecretKey != "" || b.PrivateKeyPath != ""
}

// KindNeedsRoot states, in one place, which kinds are addressed by a filesystem
// root. The API validates per kind, and the console builds its form from this
// rule: `fs` stores under `RootPath`, `sftp` writes only under `RootPath`,
// while `webdav` addresses a container (the first segment of `Prefix`) and `s3`
// a bucket — a root field on those two is a field that does nothing.
func KindNeedsRoot(kind string) bool {
	return kind == StorageKindFS || kind == StorageKindSFTP
}

// KindNeedsBucket states which kinds are an object store.
func KindNeedsBucket(kind string) bool { return kind == StorageKindS3 }
