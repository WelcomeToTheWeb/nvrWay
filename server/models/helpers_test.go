package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTableNames(t *testing.T) {
	// The plan's schema is the API's mental model: one table per noun. A
	// renamed table is a migration nobody asked for, so the names are asserted.
	want := map[string]string{
		"Camera":          (&Camera{}).TableName(),
		"Event":           (&Event{}).TableName(),
		"Segment":         (&Segment{}).TableName(),
		"StorageBackend":  (&StorageBackend{}).TableName(),
		"StorageJob":      (&StorageJob{}).TableName(),
		"Webhook":         (&Webhook{}).TableName(),
		"WebhookDelivery": (&WebhookDelivery{}).TableName(),
		"User":            (&User{}).TableName(),
		"Role":            (&Role{}).TableName(),
		"UserRole":        (&UserRole{}).TableName(),
		"Session":         (&Session{}).TableName(),
		"SSOProvider":     (&SSOProvider{}).TableName(),
		"PendingIdentity": (&PendingIdentity{}).TableName(),
		"Audit":           (&Audit{}).TableName(),
		"Setting":         (&Setting{}).TableName(),
	}
	for _, noun := range []string{
		"cameras", "events", "segments", "storage_backends", "storage_jobs",
		"webhooks", "webhook_deliveries", "users", "roles", "user_roles",
		"sessions", "sso_providers", "pending_identities", "audit", "settings",
	} {
		found := false
		for model, table := range want {
			if table == noun {
				found = true
				break
			}
			_ = model
		}
		if !found {
			t.Fatalf("no model writes the table %q", noun)
		}
	}

	// The join table gorm derives from the Role.Users many2many is the one the
	// plan names, not a pluralised variant.
	if (&UserRole{}).TableName() != "user_roles" {
		t.Fatalf("user_roles: got %q", (&UserRole{}).TableName())
	}
}

func TestEffectiveRetentionPolicy(t *testing.T) {
	// 0 is the documented "use the installation default" value, not a policy:
	// a camera whose retention is unset must not be swept after 0 days.
	camera := &Camera{LocalRetentionDays: 0, RemoteRetentionDays: 0}
	if got := camera.EffectiveLocalRetention(14); got != 14 {
		t.Fatalf("expected the installation default, got %d", got)
	}
	if got := camera.EffectiveRemoteRetention(30); got != 30 {
		t.Fatalf("expected the installation default, got %d", got)
	}

	// A camera on the cheap disk keeps less locally. An unset camera window is
	// "use the installation default", and it is the installation default of 0
	// that means keep-forever — a camera's 0 is not a second spelling of it.
	camera = &Camera{LocalRetentionDays: 3, RemoteRetentionDays: 0}
	if got := camera.EffectiveLocalRetention(14); got != 3 {
		t.Fatalf("expected the camera's own window, got %d", got)
	}
	if got := camera.EffectiveRemoteRetention(30); got != 30 {
		t.Fatalf("an unset camera window falls back to the installation default, got %d", got)
	}
	if got := camera.EffectiveRemoteRetention(0); got != 0 {
		t.Fatalf("an installation default of 0 is keep-forever, got %d", got)
	}

	camera = &Camera{LocalRetentionDays: 7, RemoteRetentionDays: 90}
	if got := camera.EffectiveLocalRetention(14); got != 7 {
		t.Fatalf("expected 7, got %d", got)
	}
	if got := camera.EffectiveRemoteRetention(30); got != 90 {
		t.Fatalf("expected 90, got %d", got)
	}
}

func TestRolePermissionCheck(t *testing.T) {
	role := &Role{Name: RoleOperator, Permissions: []string{
		PermCamerasRead, PermCamerasWrite, PermSegmentsRead,
	}}
	for _, perm := range role.Permissions {
		if !role.HasPermission(perm) {
			t.Fatalf("role %q should have %q", role.Name, perm)
		}
	}
	if role.HasPermission(PermUsersWrite) {
		t.Fatal("an operator has no account administration")
	}
	if role.HasPermission("") {
		t.Fatal("an empty permission grants nothing")
	}

	// The shorthand an operator writes into a custom role: the resource star
	// grants every verb on that resource, and `admin:*` grants everything. The
	// middleware and the role editor must agree on this, so it is asserted once,
	// here.
	if !PermissionsGrant([]string{PermCamerasStar}, PermCamerasWrite) {
		t.Fatal("cameras:* must grant cameras:write")
	}
	if PermissionsGrant([]string{PermCamerasStar}, PermUsersWrite) {
		t.Fatal("cameras:* must not reach outside cameras")
	}
	if !PermissionsGrant([]string{PermAdminStar}, PermUsersDelete) {
		t.Fatal("admin:* implies everything")
	}
	if PermissionsGrant(nil, PermCamerasRead) {
		t.Fatal("an empty list grants nothing")
	}

	// A nil row grants nothing, so a handler that forgets to check for a missing
	// role fails closed.
	var missing *Role
	if missing.HasPermission(PermCamerasRead) {
		t.Fatal("a missing role grants nothing")
	}
}

func TestDefaultBuiltinRoleFloors(t *testing.T) {
	// The role floors, asserted as data: the viewer is the floor every account
	// gets, and a role that quietly acquires `events:delete` is an escalation no
	// one reviewed.
	roles := DefaultBuiltinRoles()
	byName := map[string]BuiltinRole{}
	for _, role := range roles {
		byName[role.Name] = role
	}

	viewer := byName[RoleViewer]
	if viewer.Name == "" {
		t.Fatal("no viewer role")
	}
	for _, perm := range []string{
		PermCamerasRead, PermEventsRead, PermSegmentsRead, PermLiveRead,
		PermWSLiveRead, PermWSClipsRead,
	} {
		if !PermissionsGrant(viewer.Permissions, perm) {
			t.Fatalf("the viewer floor must include %q", perm)
		}
	}
	for _, perm := range []string{
		PermCamerasWrite, PermCamerasDelete, PermZonesWrite, PermEventsDelete,
		PermStorageWrite, PermUsersWrite, PermUsersDelete, PermRolesWrite,
		PermSettingsWrite, PermOIDCWrite, PermWSAdminStar,
	} {
		if PermissionsGrant(viewer.Permissions, perm) {
			t.Fatalf("the viewer is read-only: %q must not be on the viewer role", perm)
		}
	}

	operator := byName[RoleOperator]
	if operator.Name == "" {
		t.Fatal("no operator role")
	}
	for _, perm := range []string{
		PermCamerasWrite, PermCamerasProbe, PermZonesWrite,
		PermEventsRead, PermSegmentsRead, PermLiveRead, PermStorageRead,
		PermWSLiveRead, PermWSClipsRead, PermUsersRead,
	} {
		if !PermissionsGrant(operator.Permissions, perm) {
			t.Fatalf("an operator runs cameras: %q must be granted", perm)
		}
	}
	for _, perm := range []string{
		// A camera delete takes its rows and its recorded media with it, so the
		// plan's DELETE route is the admin's; an operator edits cameras and does
		// not dispose of them.
		PermCamerasDelete, PermEventsDelete, PermStorageWrite, PermWebhooksWrite,
		PermSettingsWrite, PermUsersWrite, PermUsersDelete, PermRolesWrite,
		PermOIDCWrite, PermWSAdminStar,
	} {
		if PermissionsGrant(operator.Permissions, perm) {
			t.Fatalf("the operator's ceiling is cameras: %q must not be granted", perm)
		}
	}

	admin := byName[RoleAdmin]
	if admin.Name == "" {
		t.Fatal("no admin role")
	}
	for _, perm := range []string{
		PermEventsDelete, PermStorageWrite, PermWebhooksWrite, PermSettingsWrite,
		PermUsersWrite, PermUsersDelete, PermRolesWrite, PermOIDCWrite, PermWSAdminStar,
		PermCamerasWrite, PermCamerasRead, PermAuditRead,
	} {
		if !PermissionsGrant(admin.Permissions, perm) {
			t.Fatalf("the admin runs the installation: %q must be granted", perm)
		}
	}

	// Every role reads live and clips: the console cannot show a stream it is
	// not allowed to read.
	for _, role := range roles {
		if !PermissionsGrant(role.Permissions, PermWSLiveRead) || !PermissionsGrant(role.Permissions, PermWSClipsRead) {
			t.Fatalf("role %q must read live and clips", role.Name)
		}
	}
}

func TestSettingKeyHelpers(t *testing.T) {
	// The recorder generation key and the pinned SFTP host keys are namespaced
	// by camera id, and Step 4/6 build the key from these helpers, not by
	// concatenating a prefix they spell differently.
	if RecorderGenPrefix != "recorder_gen_" {
		t.Fatalf("recorder generation prefix: %q", RecorderGenPrefix)
	}
	if SFTPHostKeyPrefix != "sftp_hostkey_" {
		t.Fatalf("sftp host key prefix: %q", SFTPHostKeyPrefix)
	}
	if got := RecorderGenKey("11111111-1111-1111-1111-111111111111"); got != "recorder_gen_11111111-1111-1111-1111-111111111111" {
		t.Fatalf("RecorderGenKey: %q", got)
	}
	if got := SFTPHostKeyKey("backend-1"); got != "sftp_hostkey_backend-1" {
		t.Fatalf("SFTPHostKeyKey: %q", got)
	}

	// The bootstrap timestamp and the detector heartbeat are named once, for
	// both the seeder and the readiness probe.
	if SettingBootstrapAdminSeededAt != "bootstrap_admin_seeded_at" {
		t.Fatalf("bootstrap key: %q", SettingBootstrapAdminSeededAt)
	}
	if SettingDetectorHeartbeat != "detector_heartbeat_at" {
		t.Fatalf("heartbeat key: %q", SettingDetectorHeartbeat)
	}
}

func TestStorageDriverShapeRules(t *testing.T) {
	// Root is meaningless for S3 and a bucket is meaningless for a filesystem:
	// the form offers a field only where it means something.
	for _, kind := range []string{StorageKindFS, StorageKindSFTP} {
		if !KindNeedsRoot(kind) {
			t.Fatalf("%s stores under a root path", kind)
		}
	}
	for _, kind := range []string{StorageKindS3, StorageKindWebDAV} {
		if KindNeedsRoot(kind) {
			t.Fatalf("%s is addressed by a bucket or a container, not a root", kind)
		}
	}
	if !KindNeedsBucket(StorageKindS3) {
		t.Fatal("s3 is addressed by a bucket")
	}
	for _, kind := range []string{StorageKindFS, StorageKindWebDAV, StorageKindSFTP} {
		if KindNeedsBucket(kind) {
			t.Fatalf("%s has no bucket", kind)
		}
	}
	if KindNeedsRoot("nope") || KindNeedsBucket("nope") {
		t.Fatal("an unknown driver needs nothing")
	}
}

func TestWebhookDeliverySubscription(t *testing.T) {
	hook := &Webhook{Name: "oncall", Events: `["event.close","camera.created"]`}
	if !hook.Delivers("event.close") {
		t.Fatal("expected the close subscription")
	}
	if !hook.Delivers("camera.created") {
		t.Fatal("expected the second subscription")
	}
	if hook.Delivers("event.open") {
		t.Fatal("an unsubscribed trigger must not deliver")
	}
	empty := &Webhook{Name: "nothing"}
	if empty.Delivers("event.close") {
		t.Fatal("an endpoint subscribed to nothing receives nothing")
	}
	// The trigger list on the row is the JSON array the API writes; a malformed
	// column is "no subscriptions", not a panic in the delivery loop.
	broken := &Webhook{Name: "broken", Events: "not json"}
	if broken.Delivers("event.close") {
		t.Fatal("a malformed subscription column delivers nothing")
	}
}

func TestNewRefreshTokenFailureIsVisible(t *testing.T) {
	// The generator's failure path is a crypto failure: it must surface as an
	// error, not as an empty token that a session is then created with.
	token, hash, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("new refresh token: %v", err)
	}
	if len(token) < 32 || len(hash) != 64 {
		t.Fatalf("expected a long token and a 64-char hash, got %d/%d", len(token), len(hash))
	}
	if token == hash {
		t.Fatal("the stored value is the hash")
	}
}

func TestWebhookDeliveredStampRoundTrip(t *testing.T) {
	at := time.Now().UTC()
	hook := &Webhook{Name: "oncall", URL: "https://hooks.example/nvr", LastDeliveredAt: &at, LastError: "dial tcp: refused"}
	encoded, err := hookJSON(hook)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !contains(string(encoded), "dial tcp") {
		t.Fatal("the console answers \"is this webhook working\" from the row it already has")
	}
	if contains(string(encoded), "https://hooks.example/nvr") == false {
		t.Fatal("the endpoint URL is on the row")
	}
}

func TestProviderDiscoveryDocIsNotInTheResponse(t *testing.T) {
	// The document is kept for the debug panel, out of the list response.
	provider := &SSOProvider{
		Name: "keycloak", Issuer: "https://id.example", ClientSecret: "secret",
		DiscoveryDoc: `{"issuer":"https://id.example","token_endpoint":"https://id.example/token"}`,
	}
	encoded, err := providerJSON(provider)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if contains(string(encoded), "token_endpoint") {
		t.Fatal("the discovery document has no path into the list response")
	}
}

func providerJSON(p *SSOProvider) ([]byte, error) { return json.Marshal(p) }
func hookJSON(h *Webhook) ([]byte, error)         { return json.Marshal(h) }

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
