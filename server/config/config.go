// Package config reads the installation's configuration from the environment.
//
// Every variable, its default and its meaning is in `docs/configuration.md`;
// this file is that table in code. Two habits the shape follows:
//
//   - A value that is not a number is not silently read as one. The server
//     prints what it received and uses the default, because an operator's
//     retention policy becoming a rumour (M11) starts exactly this way.
//   - Zero is a real value, not an absent one: REMOTE_RETENTION_DAYS=0 means
//     "keep remote forever" and RATE_LIMIT_BURST=0 means "a tenth of the
//     budget". The integer reader therefore accepts 0.
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Version and GitCommit are stamped at build time via
//
//	-ldflags "-X nvr/server/config.Version=x.y.z -X nvr/server/config.GitCommit=<sha>"
//
// "dev"/"unknown" means an unstamped local build. /health reports them, so an
// operator can tell which build answered them.
var (
	Version   = "dev"
	GitCommit = "unknown"
)

// WSPath is the console's WebSocket endpoint. It is a constant, not an env var:
// the console hard-codes `/ws` in its proxy and its store, and a deployment
// that moved one without the other has a console with no live updates.
const WSPath = "/ws"

// Config holds all server configuration loaded from environment variables.
type Config struct {
	ServerPort  string
	DatabaseURL string

	// JWTSecret signs access tokens. Empty means Load minted a fresh random
	// key for this boot, which is stated in the log: sessions die with the
	// process, and an operator should know that before a restart surprises them.
	JWTSecret string
	// JWTSecretIsEphemeral records that the key above came from the random
	// fallback. The boot line reads it, and so does anything that has to warn
	// about a restart ending every session.
	JWTSecretIsEphemeral bool

	// SecretKey is NVR_SECRET_KEY: 64 hex chars sealing storage-backend
	// credentials (AES-256-GCM, see Step 7's storage/seal.go). Empty falls back
	// to SHA-256(JWT_SECRET) so a deployment has exactly one secret to back up.
	SecretKey string

	// Media tree. SEGMENTS_DIR/CLIPS_DIR/SNAPSHOTS_DIR are derived from
	// DATA_DIR when unset, so the single knob a deployment usually needs is
	// DATA_DIR.
	DataDir      string
	SegmentsDir  string
	ClipsDir     string
	SnapshotsDir string

	// go2rtc: the media sidecar the recorder and the detector both read from.
	Go2rtcURL         string
	Go2rtcUsername    string
	Go2rtcPassword    string
	Go2rtcRTSPURL     string
	Go2rtcPublicURL   string
	Go2rtcMjpegWidth  int
	Go2rtcMjpegHeight int
	Go2rtcMjpegFPS    int

	// WorkerToken is the bearer the detection worker presents on
	// /api/internal/*. Empty is the internal API switched off, not unauthenticated.
	WorkerToken string

	// The break-glass local admin. An empty password means no local account is
	// created — an installation logs in through its IdP, or not at all.
	BootstrapAdminUser     string
	BootstrapAdminPassword string

	// OIDC seeds the provider row `oidc` on first boot. Discovery runs at boot
	// and a failure is a log line, never a fatal: a reachable IdP is not a
	// precondition for recording.
	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCRoleClaim    string
	OIDCRoleMapping  string

	// Recording windows.
	SegmentSeconds   int
	PreRollSeconds   int
	PostRollSeconds  int
	EventMaxSeconds  int
	UploaderTickSecs int

	// Retention, in days. RemoteRetentionDays == 0 means keep remote forever.
	LocalRetentionDays  int
	RemoteRetentionDays int
	EventRetentionDays  int

	// RateLimitPerMinute is the sustained per-user REST budget the console is
	// designed to live inside; RateLimitBurst caps the instantaneous burst
	// (0 = one tenth of the budget).
	RateLimitPerMinute int
	RateLimitBurst     int

	// WSOrigins is the comma-separated list of allowed WebSocket origins.
	// Empty falls back to WebURL (single-host installs).
	WSOrigins string
	WebURL    string

	// DetProviders is the comma-separated ONNX runtime provider list forwarded
	// verbatim in the detector config payload. CPU is the verified baseline.
	DetProviders string

	TZ string
}

// Load reads configuration from environment variables, applying defaults.
func Load() *Config {
	jwtSecret := getEnv("JWT_SECRET", "")
	ephemeral := false
	if jwtSecret == "" {
		// No shared default secret: every boot gets a fresh random key.
		// Cost: sessions do not survive a server restart. Set JWT_SECRET for
		// sticky sessions.
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			log.Fatalf("config: generating JWT secret: %v", err)
		}
		jwtSecret = hex.EncodeToString(buf)
		ephemeral = true
	}

	dataDir := getEnv("DATA_DIR", "/var/lib/nvr")

	cfg := &Config{
		ServerPort:           getEnv("SERVER_PORT", ":8080"),
		DatabaseURL:          getEnv("DATABASE_URL", "postgresql://nvr:nvr@localhost:5432/nvr?sslmode=disable"),
		JWTSecret:            jwtSecret,
		JWTSecretIsEphemeral: ephemeral,
		SecretKey:            getEnv("NVR_SECRET_KEY", ""),

		DataDir:      dataDir,
		SegmentsDir:  getEnv("SEGMENTS_DIR", filepath.Join(dataDir, "segments")),
		ClipsDir:     getEnv("CLIPS_DIR", filepath.Join(dataDir, "clips")),
		SnapshotsDir: getEnv("SNAPSHOTS_DIR", filepath.Join(dataDir, "snapshots")),

		Go2rtcURL:         getEnv("GO2RTC_URL", "http://go2rtc:1984"),
		Go2rtcUsername:    getEnv("GO2RTC_USERNAME", ""),
		Go2rtcPassword:    getEnv("GO2RTC_PASSWORD", ""),
		Go2rtcRTSPURL:     getEnv("GO2RTC_RTSP_URL", "rtsp://go2rtc:8554"),
		Go2rtcPublicURL:   getEnv("GO2RTC_PUBLIC_URL", "http://localhost:1984"),
		Go2rtcMjpegWidth:  getEnvInt("GO2RTC_MJPEG_WIDTH", 640),
		Go2rtcMjpegHeight: getEnvInt("GO2RTC_MJPEG_HEIGHT", 360),
		Go2rtcMjpegFPS:    getEnvInt("GO2RTC_MJPEG_FPS", 5),

		WorkerToken: getEnv("WORKER_TOKEN", ""),

		BootstrapAdminUser:     getEnv("BOOTSTRAP_ADMIN_USER", "admin"),
		BootstrapAdminPassword: getEnv("BOOTSTRAP_ADMIN_PASSWORD", ""),

		OIDCIssuer:       getEnv("OIDC_ISSUER", ""),
		OIDCClientID:     getEnv("OIDC_CLIENT_ID", ""),
		OIDCClientSecret: getEnv("OIDC_CLIENT_SECRET", ""),
		OIDCRoleClaim:    getEnv("OIDC_ROLE_CLAIM", "groups"),
		OIDCRoleMapping:  getEnv("OIDC_ROLE_MAPPING", ""),

		SegmentSeconds:   getEnvInt("SEGMENT_SECONDS", 5),
		PreRollSeconds:   getEnvInt("PRE_ROLL_SECONDS", 8),
		PostRollSeconds:  getEnvInt("POST_ROLL_SECONDS", 15),
		EventMaxSeconds:  getEnvInt("EVENT_MAX_SECONDS", 300),
		UploaderTickSecs: getEnvInt("UPLOADER_TICK_SECONDS", 30),

		LocalRetentionDays:  getEnvInt("LOCAL_RETENTION_DAYS", 14),
		RemoteRetentionDays: getEnvInt("REMOTE_RETENTION_DAYS", 30),
		EventRetentionDays:  getEnvInt("EVENT_RETENTION_DAYS", 90),

		RateLimitPerMinute: getEnvInt("RATE_LIMIT_PER_MINUTE", 120),
		RateLimitBurst:     getEnvInt("RATE_LIMIT_BURST", 0),

		WSOrigins: getEnv("WS_ORIGINS", ""),
		WebURL:    getEnv("WEB_URL", "http://localhost:3000"),

		DetProviders: getEnv("DET_PROVIDERS", "CPUExecutionProvider"),

		TZ: getEnv("TZ", "UTC"),
	}

	if cfg.RateLimitBurst == 0 {
		// 0 is the documented "a tenth of the budget", not "no burst".
		cfg.RateLimitBurst = cfg.RateLimitPerMinute / 10
	}

	if ephemeral {
		log.Printf("WARNING: JWT_SECRET is not set; using an ephemeral random secret. Every session ends when this process exits.")
	}
	if cfg.SecretKey == "" {
		log.Printf("WARNING: NVR_SECRET_KEY is not set; sealing storage credentials with a key derived from JWT_SECRET. " +
			"Set NVR_SECRET_KEY (openssl rand -hex 32) so sealed rows survive a JWT secret rotation. See docs/configuration.md.")
	}

	return cfg
}

// LocalAdminEnabled reports whether a break-glass local admin account can be
// created: it needs a password. The boot line states the answer, because "how
// do I get in the first time" is a question the log should answer.
func (c *Config) LocalAdminEnabled() bool {
	return c != nil && c.BootstrapAdminPassword != ""
}

// OIDCConfigured reports whether the env-seeded provider has enough to try
// discovery. All three are required: an issuer without a client is not a
// provider, it is a URL.
func (c *Config) OIDCConfigured() bool {
	return c != nil && c.OIDCIssuer != "" && c.OIDCClientID != "" && c.OIDCClientSecret != ""
}

// DetProviderList splits DET_PROVIDERS into the list the detector config
// payload carries verbatim. Empty entries are dropped so a trailing comma does
// not become a provider named "".
func (c *Config) DetProviderList() []string {
	var out []string
	for _, p := range strings.Split(c.DetProviders, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{"CPUExecutionProvider"}
	}
	return out
}

// SealKey returns the 32-byte AES-256-GCM key sealing storage-backend
// credentials: NVR_SECRET_KEY decoded as hex, or SHA-256(JWT_SECRET) when it is
// unset. A 64-hex value that is not hex is not silently hashed — it is the
// common typo for a key copied from `openssl rand -hex 32` with a newline, and
// reading it as "unset" would silently change which key seals the rows.
func (c *Config) SealKey() ([32]byte, error) {
	var key [32]byte
	if raw := strings.TrimSpace(c.SecretKey); raw != "" {
		decoded, err := hex.DecodeString(raw)
		if err != nil {
			return key, fmt.Errorf("config: NVR_SECRET_KEY must be hex: %w", err)
		}
		if len(decoded) != 32 {
			return key, fmt.Errorf("config: NVR_SECRET_KEY must decode to 32 bytes, got %d", len(decoded))
		}
		copy(key[:], decoded)
		return key, nil
	}
	return [32]byte(sha256.Sum256([]byte(c.JWTSecret))), nil
}

// EnsureDirs creates the media directories. The recorder, the clip builder and
// the snapshot writer all assume their directory exists, and a media pipeline
// that fails on a missing directory at the first event is a failure an operator
// sees as "no clip" with no reason.
func (c *Config) EnsureDirs() error {
	for _, dir := range []string{c.DataDir, c.SegmentsDir, c.ClipsDir, c.SnapshotsDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("config: creating %s: %w", dir, err)
		}
	}
	return nil
}

// getEnvInt reads a non-negative integer. Zero is a value (keep remote forever,
// no burst), so it is accepted; an unparseable value is stated in the log and
// the default is used.
func getEnvInt(key string, fallback int) int {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		log.Printf("config: %s=%q is not a non-negative number; using the default %d", key, raw, fallback)
		return fallback
	}
	return n
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
