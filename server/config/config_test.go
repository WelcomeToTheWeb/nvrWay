package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withEnv pins the environment a Load() sees, including the absence of every
// variable the test does not set. Two of them (JWT_SECRET, NVR_SECRET_KEY)
// change which key the installation signs and seals with, so a test that
// inherits them from the developer's shell is not testing the defaults.
func withEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	for _, key := range envKeys {
		// t.Setenv registers its own cleanup, and a key the test does not set
		// must be absent, not empty: getEnv reads an empty value as a value, and
		// the path under test is the default one.
		os.Unsetenv(key)
		if v, ok := vars[key]; ok {
			t.Setenv(key, v)
		}
	}
}

// envKeys is every variable config reads. A test that inherits any of them
// from the developer's shell is not testing the defaults, which is why the
// list is exhaustive rather than the ones each test happens to care about.
var envKeys = []string{
	"SERVER_PORT", "DATABASE_URL", "JWT_SECRET", "NVR_SECRET_KEY",
	"DATA_DIR", "SEGMENTS_DIR", "CLIPS_DIR", "SNAPSHOTS_DIR",
	"GO2RTC_URL", "GO2RTC_USERNAME", "GO2RTC_PASSWORD", "GO2RTC_RTSP_URL",
	"GO2RTC_PUBLIC_URL", "GO2RTC_MJPEG_WIDTH", "GO2RTC_MJPEG_HEIGHT", "GO2RTC_MJPEG_FPS",
	"WORKER_TOKEN", "BOOTSTRAP_ADMIN_USER", "BOOTSTRAP_ADMIN_PASSWORD",
	"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_ROLE_CLAIM", "OIDC_ROLE_MAPPING",
	"SEGMENT_SECONDS", "PRE_ROLL_SECONDS", "POST_ROLL_SECONDS", "EVENT_MAX_SECONDS",
	"LOCAL_RETENTION_DAYS", "REMOTE_RETENTION_DAYS", "EVENT_RETENTION_DAYS", "UPLOADER_TICK_SECONDS",
	"RATE_LIMIT_PER_MINUTE", "RATE_LIMIT_BURST", "WS_ORIGINS", "WEB_URL", "DET_PROVIDERS", "TZ",
	"WS_PATH",
}

func TestLoadDefaultsAreThePlanTable(t *testing.T) {
	// docs/configuration.md is this table. The plan's Step 1 says the two are
	// the same list, so every default in the table is asserted here: a default
	// that drifts from the documented one is a deployment running a policy the
	// operator read somewhere else.
	withEnv(t, nil)
	cfg := Load()

	if cfg.ServerPort != ":8080" {
		t.Fatalf("SERVER_PORT: expected :8080, got %q", cfg.ServerPort)
	}
	if cfg.DatabaseURL != "postgresql://nvr:nvr@localhost:5432/nvr?sslmode=disable" {
		t.Fatalf("DATABASE_URL default: %q", cfg.DatabaseURL)
	}
	if cfg.DataDir != "/var/lib/nvr" {
		t.Fatalf("DATA_DIR default: %q", cfg.DataDir)
	}
	// The three media directories derive from DATA_DIR when unset, so the single
	// knob a deployment usually needs is DATA_DIR.
	if cfg.SegmentsDir != filepath.Join("/var/lib/nvr", "segments") {
		t.Fatalf("SEGMENTS_DIR must derive from DATA_DIR: %q", cfg.SegmentsDir)
	}
	if cfg.ClipsDir != filepath.Join("/var/lib/nvr", "clips") {
		t.Fatalf("CLIPS_DIR must derive from DATA_DIR: %q", cfg.ClipsDir)
	}
	if cfg.SnapshotsDir != filepath.Join("/var/lib/nvr", "snapshots") {
		t.Fatalf("SNAPSHOTS_DIR must derive from DATA_DIR: %q", cfg.SnapshotsDir)
	}

	if cfg.Go2rtcURL != "http://go2rtc:1984" {
		t.Fatalf("GO2RTC_URL default: %q", cfg.Go2rtcURL)
	}
	if cfg.Go2rtcRTSPURL != "rtsp://go2rtc:8554" {
		t.Fatalf("GO2RTC_RTSP_URL default: %q", cfg.Go2rtcRTSPURL)
	}
	if cfg.Go2rtcPublicURL != "http://localhost:1984" {
		t.Fatalf("GO2RTC_PUBLIC_URL default: %q", cfg.Go2rtcPublicURL)
	}
	if cfg.Go2rtcUsername != "" || cfg.Go2rtcPassword != "" {
		t.Fatal("go2rtc basic auth is off by default")
	}
	if cfg.Go2rtcMjpegWidth != 640 || cfg.Go2rtcMjpegHeight != 360 || cfg.Go2rtcMjpegFPS != 5 {
		t.Fatalf("detection tap defaults are 640x360@5, got %dx%d@%d",
			cfg.Go2rtcMjpegWidth, cfg.Go2rtcMjpegHeight, cfg.Go2rtcMjpegFPS)
	}

	if cfg.WorkerToken != "" {
		t.Fatal("WORKER_TOKEN has no default: an empty token is the internal API switched off")
	}
	if cfg.BootstrapAdminUser != "admin" {
		t.Fatalf("BOOTSTRAP_ADMIN_USER default: %q", cfg.BootstrapAdminUser)
	}
	if cfg.BootstrapAdminPassword != "" {
		t.Fatal("BOOTSTRAP_ADMIN_PASSWORD has no default: no local admin is created")
	}
	if cfg.OIDCIssuer != "" || cfg.OIDCClientID != "" || cfg.OIDCClientSecret != "" {
		t.Fatal("OIDC is configured by the deployment, not by a default")
	}
	if cfg.OIDCRoleClaim != "groups" {
		t.Fatalf("OIDC_ROLE_CLAIM default: %q", cfg.OIDCRoleClaim)
	}
	if cfg.OIDCRoleMapping != "" {
		t.Fatal("OIDC_ROLE_MAPPING empty means no mapping and the viewer floor stands")
	}

	if cfg.SegmentSeconds != 5 {
		t.Fatalf("SEGMENT_SECONDS default: %d", cfg.SegmentSeconds)
	}
	if cfg.PreRollSeconds != 8 || cfg.PostRollSeconds != 15 {
		t.Fatalf("event window defaults are 8/15, got %d/%d", cfg.PreRollSeconds, cfg.PostRollSeconds)
	}
	if cfg.EventMaxSeconds != 300 {
		t.Fatalf("EVENT_MAX_SECONDS default: %d", cfg.EventMaxSeconds)
	}
	if cfg.UploaderTickSecs != 30 {
		t.Fatalf("UPLOADER_TICK_SECONDS default: %d", cfg.UploaderTickSecs)
	}

	if cfg.LocalRetentionDays != 14 {
		t.Fatalf("LOCAL_RETENTION_DAYS default: %d", cfg.LocalRetentionDays)
	}
	if cfg.RemoteRetentionDays != 30 {
		t.Fatalf("REMOTE_RETENTION_DAYS default: %d", cfg.RemoteRetentionDays)
	}
	if cfg.EventRetentionDays != 90 {
		t.Fatalf("EVENT_RETENTION_DAYS default: %d", cfg.EventRetentionDays)
	}

	if cfg.RateLimitPerMinute != 120 {
		t.Fatalf("RATE_LIMIT_PER_MINUTE default: %d", cfg.RateLimitPerMinute)
	}
	if cfg.RateLimitBurst != 12 {
		t.Fatalf("RATE_LIMIT_BURST 0 means a tenth of the budget: expected 12, got %d", cfg.RateLimitBurst)
	}

	if cfg.WSOrigins != "" {
		t.Fatal("WS_ORIGINS empty falls back to WEB_URL in the hub, not here")
	}
	if cfg.WebURL != "http://localhost:3000" {
		t.Fatalf("WEB_URL default: %q", cfg.WebURL)
	}
	if cfg.DetProviders != "CPUExecutionProvider" {
		t.Fatalf("DET_PROVIDERS default: %q (CPU is the verified baseline)", cfg.DetProviders)
	}
	if cfg.TZ != "UTC" {
		t.Fatalf("TZ default: %q", cfg.TZ)
	}

	// The WebSocket endpoint is a constant, not an env var: the console hard
	// codes it in its proxy and its store.
	if WSPath != "/ws" {
		t.Fatalf("WSPath: %q", WSPath)
	}
}

func TestLoadJWTSecretIsEmptyRandomAndSaysSo(t *testing.T) {
	withEnv(t, nil)
	cfg := Load()
	if cfg.JWTSecret == "" {
		t.Fatal("expected a generated secret, not an empty signing key")
	}
	if len(cfg.JWTSecret) != 64 {
		t.Fatalf("expected a 32-byte hex key, got %d chars", len(cfg.JWTSecret))
	}
	if !cfg.JWTSecretIsEphemeral {
		t.Fatal("the ephemeral fallback must be recorded: the boot line states the cost")
	}

	first := Load()
	second := Load()
	if first.JWTSecret == second.JWTSecret {
		t.Fatal("the random fallback must differ per boot — that is the cost the log states")
	}

	withEnv(t, map[string]string{"JWT_SECRET": "a-stable-secret"})
	stable := Load()
	if stable.JWTSecret != "a-stable-secret" {
		t.Fatalf("expected the configured secret, got %q", stable.JWTSecret)
	}
	if stable.JWTSecretIsEphemeral {
		t.Fatal("a configured secret is not ephemeral")
	}
}

func TestLoadZeroIsAValueNotAnAbsence(t *testing.T) {
	// Two documented behaviours read 0 as a number: REMOTE_RETENTION_DAYS=0
	// keeps remote footage forever, and RATE_LIMIT_BURST=0 means a tenth of the
	// budget. An integer reader that treats 0 as missing turns "keep forever"
	// into "delete after 30 days", which is the failure an operator cannot undo.
	withEnv(t, map[string]string{
		"REMOTE_RETENTION_DAYS":    "0",
		"RATE_LIMIT_BURST":         "0",
		"RATE_LIMIT_PER_MINUTE":    "1000",
		"SEGMENT_SECONDS":          "10",
		"GO2RTC_MJPEG_FPS":         "0",
		"BOOTSTRAP_ADMIN_PASSWORD": "",
	})
	cfg := Load()

	if cfg.RemoteRetentionDays != 0 {
		t.Fatalf("REMOTE_RETENTION_DAYS=0 must read as 0 (keep remote forever), got %d", cfg.RemoteRetentionDays)
	}
	if cfg.RateLimitPerMinute != 1000 {
		t.Fatalf("expected the raised e2e budget, got %d", cfg.RateLimitPerMinute)
	}
	if cfg.RateLimitBurst != 100 {
		t.Fatalf("expected a tenth of 1000, got %d", cfg.RateLimitBurst)
	}
	if cfg.SegmentSeconds != 10 {
		t.Fatalf("expected the 10 s segment length, got %d", cfg.SegmentSeconds)
	}
	if cfg.Go2rtcMjpegFPS != 0 {
		t.Fatalf("an explicit 0 must read as 0, got %d", cfg.Go2rtcMjpegFPS)
	}
	if cfg.LocalAdminEnabled() {
		t.Fatal("an empty bootstrap password creates no local admin")
	}
}

func TestLoadUnparseableNumberFallsBackAndIsNotSilent(t *testing.T) {
	withEnv(t, map[string]string{"LOCAL_RETENTION_DAYS": "fourteen", "SEGMENT_SECONDS": "-5"})
	cfg := Load()

	if cfg.LocalRetentionDays != 14 {
		t.Fatalf("expected the documented default for an unparseable value, got %d", cfg.LocalRetentionDays)
	}
	// A negative retention is not a policy: it would delete everything the
	// first sweep runs.
	if cfg.SegmentSeconds != 5 {
		t.Fatalf("expected the default for a negative segment length, got %d", cfg.SegmentSeconds)
	}
}

func TestLoadDerivesDirsFromDataDirOnlyWhenUnset(t *testing.T) {
	root := t.TempDir()

	withEnv(t, map[string]string{"DATA_DIR": root})
	cfg := Load()
	if cfg.SegmentsDir != filepath.Join(root, "segments") {
		t.Fatalf("expected the derived segments dir, got %q", cfg.SegmentsDir)
	}

	// An operator who puts the segments on the big disk and the clips on the
	// fast one has three knobs, and DATA_DIR must not stamp on them.
	withEnv(t, map[string]string{
		"DATA_DIR":      root,
		"SEGMENTS_DIR":  filepath.Join(root, "spindle"),
		"CLIPS_DIR":     filepath.Join(root, "nvme"),
		"SNAPSHOTS_DIR": filepath.Join(root, "snap"),
	})
	cfg = Load()
	if cfg.SegmentsDir != filepath.Join(root, "spindle") || cfg.ClipsDir != filepath.Join(root, "nvme") ||
		cfg.SnapshotsDir != filepath.Join(root, "snap") {
		t.Fatalf("explicit directories were overridden: %+v", cfg)
	}
}

func TestEnsureDirsCreatesTheMediaTree(t *testing.T) {
	root := t.TempDir()
	withEnv(t, map[string]string{"DATA_DIR": root})
	cfg := Load()

	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	for _, dir := range []string{cfg.DataDir, cfg.SegmentsDir, cfg.ClipsDir, cfg.SnapshotsDir} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("%s must exist: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
	}

	// A media pipeline that fails on a missing directory at the first event
	// reads to an operator as "no clip" with no reason, so EnsureDirs is a boot
	// step and its error is a fatal one.
	if err := (&Config{DataDir: filepath.Join(root, "deep", "nested", "tree"),
		SegmentsDir:  filepath.Join(root, "deep", "nested", "tree", "segments"),
		ClipsDir:     filepath.Join(root, "deep", "nested", "tree", "clips"),
		SnapshotsDir: filepath.Join(root, "deep", "nested", "tree", "snapshots")}).EnsureDirs(); err != nil {
		t.Fatalf("nested tree: %v", err)
	}
}

func TestSealKeyDerivation(t *testing.T) {
	// NVR_SECRET_KEY, when set, is the key: 32 bytes of hex, decoded.
	hexKey := strings.Repeat("a1", 32)
	withEnv(t, map[string]string{"NVR_SECRET_KEY": hexKey, "JWT_SECRET": "jwt"})
	cfg := Load()

	key, err := cfg.SealKey()
	if err != nil {
		t.Fatalf("seal key: %v", err)
	}
	decoded, _ := hex.DecodeString(hexKey)
	if key != [32]byte(decoded) {
		t.Fatal("expected the configured key, decoded")
	}

	// Unset falls back to SHA-256(JWT_SECRET), so a deployment has exactly one
	// secret to back up — and the fallback is deterministic across restarts,
	// which is the whole point of sealing.
	withEnv(t, map[string]string{"JWT_SECRET": "jwt"})
	cfg = Load()
	key, err = cfg.SealKey()
	if err != nil {
		t.Fatalf("seal key: %v", err)
	}
	want := sha256.Sum256([]byte("jwt"))
	if key != want {
		t.Fatal("expected SHA-256(JWT_SECRET) as the fallback seal key")
	}

	// A key that is not hex is not hashed into the fallback: that is the typo
	// for a key copied with a stray character, and reading it as "unset" would
	// silently change which key seals the rows.
	withEnv(t, map[string]string{"NVR_SECRET_KEY": strings.Repeat("z", 64), "JWT_SECRET": "jwt"})
	cfg = Load()
	if _, err := cfg.SealKey(); err == nil {
		t.Fatal("expected a non-hex NVR_SECRET_KEY to be an error")
	}

	withEnv(t, map[string]string{"NVR_SECRET_KEY": strings.Repeat("a1", 16), "JWT_SECRET": "jwt"})
	cfg = Load()
	if _, err := cfg.SealKey(); err == nil {
		t.Fatal("expected a short NVR_SECRET_KEY to be an error, not a padded key")
	}
}

func TestDetProviderList(t *testing.T) {
	withEnv(t, map[string]string{"DET_PROVIDERS": "CUDAExecutionProvider,CPUExecutionProvider"})
	cfg := Load()
	got := cfg.DetProviderList()
	if len(got) != 2 || got[0] != "CUDAExecutionProvider" || got[1] != "CPUExecutionProvider" {
		t.Fatalf("expected the provider list verbatim, got %v", got)
	}

	// Spaces and a trailing comma are a typed env file, not a provider named "".
	withEnv(t, map[string]string{"DET_PROVIDERS": " CPUExecutionProvider , "})
	cfg = Load()
	if got := cfg.DetProviderList(); len(got) != 1 || got[0] != "CPUExecutionProvider" {
		t.Fatalf("expected the trimmed list, got %q", got)
	}

	// An empty list is not "no providers": inference with no provider is a
	// worker that crashes, and CPU is the answer the plan verified on this host.
	withEnv(t, map[string]string{"DET_PROVIDERS": ""})
	cfg = Load()
	if got := cfg.DetProviderList(); len(got) != 1 || got[0] != "CPUExecutionProvider" {
		t.Fatalf("expected the CPU floor, got %v", got)
	}
}

func TestAuthStateIsAnswerable(t *testing.T) {
	withEnv(t, nil)
	cfg := Load()
	if cfg.LocalAdminEnabled() || cfg.OIDCConfigured() {
		t.Fatal("a default install has neither door")
	}

	withEnv(t, map[string]string{"BOOTSTRAP_ADMIN_PASSWORD": "hunter2"})
	cfg = Load()
	if !cfg.LocalAdminEnabled() {
		t.Fatal("a bootstrap password enables the local admin")
	}
	if cfg.OIDCConfigured() {
		t.Fatal("no OIDC configured")
	}

	// An issuer without a client is not a provider, it is a URL: all three are
	// required, and a half-configured provider that the boot line calls
	// configured is how an operator discovers at 2 a.m. that login is broken.
	withEnv(t, map[string]string{"OIDC_ISSUER": "https://id.example", "OIDC_CLIENT_ID": "nvr"})
	cfg = Load()
	if cfg.OIDCConfigured() {
		t.Fatal("OIDC needs issuer, client id and client secret")
	}

	withEnv(t, map[string]string{
		"OIDC_ISSUER":        "https://id.example",
		"OIDC_CLIENT_ID":     "nvr",
		"OIDC_CLIENT_SECRET": "secret",
	})
	cfg = Load()
	if !cfg.OIDCConfigured() {
		t.Fatal("expected the provider to read as configured")
	}
}
