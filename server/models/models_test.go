package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestJSONStringListTolerance(t *testing.T) {
	// Every JSON text column in the schema is read through this, so a column
	// written by an older release cannot panic a reader.
	for _, empty := range []string{"", "  ", "null", "not json", `{"a":1}`} {
		if got := JSONStringList(empty); got != nil {
			t.Fatalf("JSONStringList(%q) must answer no entries, got %v", empty, got)
		}
	}
	if got := JSONStringList(`["person","car"]`); len(got) != 2 || got[0] != "person" {
		t.Fatalf("expected the list back, got %v", got)
	}
	if got := JSONStringList(`[]`); len(got) != 0 {
		t.Fatalf("expected no entries, got %v", got)
	}
}

func TestJSONStringMapTolerance(t *testing.T) {
	for _, empty := range []string{"", "null", "[]", "nope"} {
		if got := JSONStringMap(empty); got != nil {
			t.Fatalf("JSONStringMap(%q) must answer nothing, got %v", empty, got)
		}
	}
	if got := JSONStringMap(`{"nvr-ops":"operator"}`); got["nvr-ops"] != "operator" {
		t.Fatalf("expected the mapping back, got %v", got)
	}
}

func TestAppendUniquePreservesOrderAndSkipsDuplicates(t *testing.T) {
	// The event row's class/zone/rule unions use this: the accumulated facts
	// keep the order they arrived in, and a signal that re-sends cannot grow
	// the list.
	got := AppendUnique([]string{"person", "car"}, []string{"car", "bus", "person"})
	if strings.Join(got, ",") != "person,car,bus" {
		t.Fatalf("expected order preserved with duplicates skipped, got %v", got)
	}
	if got := AppendUnique(nil, []string{"person", "person"}); len(got) != 1 {
		t.Fatalf("expected one entry, got %v", got)
	}
	if got := AppendUnique([]string{"person"}, nil); len(got) != 1 {
		t.Fatalf("expected the original list, got %v", got)
	}
	if got := AppendUnique(nil, nil); len(got) != 0 {
		t.Fatalf("expected nothing, got %v", got)
	}
}

func TestSecretsNeverReachJSON(t *testing.T) {
	// The API is allowed to say a secret is set, never what it is. The model
	// tags are the only thing standing between a secret and a response, and a
	// handler that answers a row directly is one refactor away from echoing
	// credentials — so the tags are asserted here, on the models.
	type secretRow struct {
		name  string
		value any
		want  []string
	}
	rows := []secretRow{
		{"StorageBackend", &StorageBackend{
			Password: "sealed-password", AccessKey: "sealed-access",
			SecretKey: "sealed-secret", PrivateKeyPath: "/sealed/path",
		}, []string{"sealed-password", "sealed-access", "sealed-secret", "/sealed/path"}},
		{"User", &User{PasswordHash: "argon2id$sealed"}, []string{"argon2id$sealed"}},
		{"Webhook", &Webhook{Secret: "shared-signing-secret"}, []string{"shared-signing-secret"}},
		{"SSOProvider", &SSOProvider{ClientSecret: "idp-client-secret", DiscoveryDoc: `{"issuer":"https://id.example"}`},
			[]string{"idp-client-secret", "https://id.example"}},
		{"Session", &Session{TokenHash: strings.Repeat("a", 64), RotatedFrom: strings.Repeat("b", 64)},
			[]string{strings.Repeat("a", 64), strings.Repeat("b", 64)}},
	}

	for _, row := range rows {
		encoded, err := json.Marshal(row.value)
		if err != nil {
			t.Fatalf("%s: marshal: %v", row.name, err)
		}
		for _, secret := range row.want {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("%s: the response body carries %q: %s", row.name, secret, encoded)
			}
		}
	}

	// The field the console is given instead.
	backend := &StorageBackend{Name: "tier-one", Kind: StorageKindFS}
	if backend.SecretSet() {
		t.Fatal("an empty backend reports no secret")
	}
	backend.Password = "sealed"
	if !backend.SecretSet() {
		t.Fatal("expected SecretSet to report a set secret")
	}
	webhook := &Webhook{Name: "oncall"}
	if webhook.SecretSet() {
		t.Fatal("an endpoint with no signing secret reports none")
	}
	webhook.Secret = "s"
	if !webhook.SecretSet() {
		t.Fatal("expected the signing secret to be reported as set")
	}
	provider := &SSOProvider{Name: "oidc"}
	if provider.SecretSet() {
		t.Fatal("a provider with no client secret reports none")
	}
	provider.ClientSecret = "s"
	if !provider.SecretSet() {
		t.Fatal("expected the client secret to be reported as set")
	}
}

func TestAuditOutcomeForStatusCode(t *testing.T) {
	// The outcome is rendered from the status code, so a ledger filter is a
	// filter and not a free-text reason three handlers spell differently.
	cases := map[int]string{
		200: AuditOutcomeOK,
		201: AuditOutcomeOK,
		204: AuditOutcomeOK,
		400: AuditOutcomeRefused,
		401: AuditOutcomeRefused,
		403: AuditOutcomeRefused,
		409: AuditOutcomeRefused,
		422: AuditOutcomeRefused,
		404: AuditOutcomeNotFound,
		500: AuditOutcomeError,
		503: AuditOutcomeError,
	}
	for status, want := range cases {
		if got := AuditOutcomeFor(status); got != want {
			t.Fatalf("status %d: expected %q, got %q", status, want, got)
		}
	}
}

func TestAuditIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := NewAuditID()
		if seen[id] {
			t.Fatalf("audit id %q issued twice", id)
		}
		seen[id] = true
		if len(id) != 36 {
			t.Fatalf("expected a UUID string, got %q", id)
		}
	}
}

func TestSessionTokenHashAndExpiry(t *testing.T) {
	token, hash, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("new refresh token: %v", err)
	}
	if token == "" || hash == "" {
		t.Fatal("expected a token and its hash")
	}
	if token == hash {
		t.Fatal("the stored value must be the hash, never the token")
	}
	if HashRefreshToken(token) != hash {
		t.Fatal("the hash returned alongside the token must be its hash")
	}
	if len(hash) != 64 {
		t.Fatalf("expected a 64-char hex digest, got %d", len(hash))
	}
	other, _, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("new refresh token: %v", err)
	}
	if other == token {
		t.Fatal("two issued tokens must not collide")
	}

	now := time.Now().UTC()
	session := &Session{TokenHash: hash, ExpiresAt: now.Add(time.Hour)}
	if !session.IsLive(now) {
		t.Fatal("a live session verifies")
	}
	at := now
	session.RevokedAt = &at
	if session.IsLive(now) {
		t.Fatal("a revoked session must not verify")
	}
	live := &Session{TokenHash: hash, ExpiresAt: now.Add(-time.Second)}
	if live.IsLive(now) {
		t.Fatal("an expired session must not verify")
	}
}

func TestAccountStateHelpers(t *testing.T) {
	local := &User{Provider: ProviderLocal, Status: UserStatusActive}
	if !local.IsLocal() || local.IsPending() {
		t.Fatal("a local active account")
	}

	oidc := &User{Provider: "keycloak", Status: UserStatusActive}
	if oidc.IsLocal() {
		t.Fatal("an OIDC account has no password to check")
	}

	pending := &User{Provider: "keycloak", Status: UserStatusPending}
	if !pending.IsPending() {
		t.Fatal("a pending identity is the state every auth path refuses")
	}
	if pending.IsLocal() {
		t.Fatal("a pending identity is never a password account")
	}

	var nilUser *User
	if nilUser.IsPending() || nilUser.IsLocal() {
		t.Fatal("a missing account is neither state, and the helpers must not panic")
	}
}

func TestSegmentKindHelpers(t *testing.T) {
	if !ValidSegmentKind(SegmentKindContinuous) || !ValidSegmentKind(SegmentKindEvent) {
		t.Fatal("both segment kinds must be valid")
	}
	if ValidSegmentKind("clip") || ValidSegmentKind("") {
		t.Fatal("an unnamed segment kind must not be storable")
	}
	seg := &Segment{}
	if !seg.HasLocalFile() {
		t.Fatal("a row with no delete stamp has its file")
	}
	now := time.Now().UTC()
	seg.LocalDeletedAt = &now
	if seg.HasLocalFile() {
		t.Fatal("a row stamped local-deleted has no file")
	}
	if seg.Uploaded() {
		t.Fatal("nothing is uploaded")
	}
	seg.UploadedAt = &now
	if !seg.Uploaded() {
		t.Fatal("expected the upload stamp to read as uploaded")
	}
}

func TestEventUploadStamps(t *testing.T) {
	event := &Event{}
	if event.SnapshotUploaded() || event.ClipUploaded() {
		t.Fatal("nil is not uploaded: the sweeper must not delete un-uploaded media")
	}
	now := time.Now().UTC()
	event.ClipUploadedAt = &now
	if !event.ClipUploaded() {
		t.Fatal("expected the clip stamp to read as uploaded")
	}
	if event.SnapshotUploaded() {
		t.Fatal("the snapshot is still local-only")
	}
}

func TestSeverityAndSignalActions(t *testing.T) {
	for _, s := range []int{SeverityLow, SeverityMedium, SeverityHigh} {
		if !ValidSeverity(s) {
			t.Fatalf("severity %d must be valid", s)
		}
	}
	for _, s := range []int{0, -1, 4, 99} {
		if ValidSeverity(s) {
			t.Fatalf("severity %d must be refused (the payload and the console ring both speak 1..3)", s)
		}
	}
	for _, action := range []string{SignalActionOpen, SignalActionExtend, SignalActionClose} {
		if !ValidSignalAction(action) {
			t.Fatalf("action %q must be valid", action)
		}
	}
	for _, action := range []string{"", "expire", "OPEN", "update"} {
		if ValidSignalAction(action) {
			t.Fatalf("action %q must be refused", action)
		}
	}
}

func TestJobConstantsAreThePlanSet(t *testing.T) {
	for _, kind := range []string{JobKindSegment, JobKindClip, JobKindSnapshot} {
		if !ValidJobKind(kind) {
			t.Fatalf("job kind %q must be valid", kind)
		}
	}
	if ValidJobKind("thumbnail") || ValidJobKind("") {
		t.Fatal("an unnamed job kind must not be enqueued")
	}
	for _, action := range []string{JobActionUpload, JobActionDeleteLocal, JobActionDeleteRemote} {
		if !ValidJobAction(action) {
			t.Fatalf("job action %q must be valid", action)
		}
	}
	if ValidJobAction("delete") || ValidJobAction("") {
		t.Fatal("an unnamed job action must not be enqueued")
	}
	// The dead-letter ceiling the plan names.
	if JobMaxAttempts != 20 {
		t.Fatalf("JobMaxAttempts is %d, the plan says 20", JobMaxAttempts)
	}
}

func TestStorageKindRules(t *testing.T) {
	if !ValidStorageKind(StorageKindS3) || !ValidStorageKind(StorageKindFS) ||
		!ValidStorageKind(StorageKindWebDAV) || !ValidStorageKind(StorageKindSFTP) {
		t.Fatal("the four plan drivers must validate")
	}
	if len(StorageKinds) != 4 {
		t.Fatalf("v1 has four drivers, got %v", StorageKinds)
	}
	if ValidStorageKind("gs") || ValidStorageKind("azure") || ValidStorageKind("") {
		t.Fatal("a driver v1 does not implement must not be selectable")
	}
}

func TestLinkPolicyAndProviderKindConstants(t *testing.T) {
	// `pending` is the default a new provider gets, and the only value v1's
	// seeded provider row carries.
	if !ValidSSOLinkPolicy(SSOLinkPolicyPending) || !ValidSSOLinkPolicy(SSOLinkPolicyLink) {
		t.Fatal("both link policies must validate")
	}
	if ValidSSOLinkPolicy("") || ValidSSOLinkPolicy("auto") || ValidSSOLinkPolicy("email") {
		t.Fatal("an unnamed link policy must not be storable: it is the difference between a login working and an account being claimed")
	}
	if SSOTypeOIDC != "oidc" {
		t.Fatal("v1 speaks OIDC only")
	}
	if SSOProviderSlug != "oidc" {
		t.Fatalf("the seeded provider row is named %q", SSOProviderSlug)
	}
}

func TestIdentityStatuses(t *testing.T) {
	for _, status := range []string{IdentityStatusPending, IdentityStatusLinked, IdentityStatusRefused, IdentityStatusApproved} {
		if status == "" {
			t.Fatal("an identity status name is empty")
		}
	}
	pending := &PendingIdentity{Status: IdentityStatusPending}
	if !pending.IsPending() {
		t.Fatal("a parked assertion is pending")
	}
	decided := &PendingIdentity{Status: IdentityStatusApproved}
	if decided.IsPending() {
		t.Fatal("a decided assertion leaves the queue")
	}
	var none *PendingIdentity
	if none.IsPending() {
		t.Fatal("a missing assertion is not pending")
	}
}

func TestRecordModesAndSlugConstantsAgreeWithTheAPIContract(t *testing.T) {
	// The console sends these strings; a change here breaks the console, so the
	// literals are asserted rather than derived.
	if RecordModeContinuous != "continuous" || RecordModeEvents != "events" || RecordModeOff != "off" {
		t.Fatalf("record modes: %v", RecordModes)
	}
	if ZoneRuleIntrusion != "intrusion" || ZoneRuleLoiter != "loiter" {
		t.Fatalf("zone rules: %v", ZoneRules)
	}
	if SegmentKindContinuous != "continuous" || SegmentKindEvent != "event" {
		t.Fatalf("segment kinds: %q %q", SegmentKindContinuous, SegmentKindEvent)
	}
	if StorageKindS3 != "s3" || StorageKindWebDAV != "webdav" || StorageKindSFTP != "sftp" || StorageKindFS != "fs" {
		t.Fatalf("storage kinds: %v", StorageKinds)
	}
	if JobActionUpload != "upload" || JobActionDeleteLocal != "delete_local" || JobActionDeleteRemote != "delete_remote" {
		t.Fatal("job actions")
	}
	if JobStatePending != "pending" || JobStateUploading != "uploading" || JobStateUploaded != "uploaded" || JobStateFailed != "failed" {
		t.Fatal("job states")
	}
	if UserStatusActive != "active" || UserStatusPending != "pending" {
		t.Fatal("account statuses")
	}
	if ProviderLocal != "local" {
		t.Fatal("the local provider name is what the login handler compares")
	}
	if RoleAdmin != "admin" || RoleOperator != "operator" || RoleViewer != "viewer" {
		t.Fatal("role names are what the OIDC role mapping writes")
	}
}

func TestEventObjectWireShape(t *testing.T) {
	// The worker sends {"class","conf","box":[x1,y1,x2,y2]} and the console
	// draws it on the snapshot; the tags are the contract.
	encoded, err := json.Marshal(EventObject{Class: "person", Conf: 0.81, Box: []float64{100, 50, 140, 150}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(encoded)
	for _, key := range []string{`"class":"person"`, `"conf":0.81`, `"box":[100,50,140,150]`} {
		if !strings.Contains(got, key) {
			t.Fatalf("expected %s in %s", key, got)
		}
	}

	var back EventObject
	if err := json.Unmarshal([]byte(`{"class":"car","conf":0.6,"box":[0,0,10,10]}`), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Class != "car" || back.Conf != 0.6 || len(back.Box) != 4 {
		t.Fatalf("decoded %+v", back)
	}
}

func TestZoneJSONTagsAreTheWireContract(t *testing.T) {
	// The names the Python worker reads (Step 5) and the console writes
	// (Step 8): a rename on either side is a silent no-op, so they are asserted
	// as literals.
	encoded, err := json.Marshal(Zone{
		ID: "z1", Name: "driveway", Polygon: [][]float64{{0.12, 0.35}, {0.8, 0.35}, {0.8, 0.95}, {0.12, 0.95}},
		Classes: []string{"person"}, Rule: ZoneRuleIntrusion, DwellSec: 30, Severity: 3,
		MinConfidence: 0.35, MinAreaRatio: 0.004, Enabled: true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(encoded)
	for _, key := range []string{
		`"id":"z1"`, `"name":"driveway"`, `"polygon"`, `"classes":["person"]`,
		`"rule":"intrusion"`, `"dwell_seconds":30`, `"severity":3`,
		`"min_confidence":0.35`, `"min_area_ratio":0.004`, `"enabled":true`,
	} {
		if !strings.Contains(got, key) {
			t.Fatalf("expected %s in %s", key, got)
		}
	}
}

func TestRoleNamesAndPermissionsAreStableStrings(t *testing.T) {
	// The OIDC role mapping writes role names, and the middleware compares
	// permission strings: both are wire values.
	builtins := DefaultBuiltinRoles()
	if len(builtins) != 3 {
		t.Fatalf("v1 ships three built-in roles, got %d", len(builtins))
	}
	names := map[string]bool{}
	for _, role := range builtins {
		names[role.Name] = true
		if len(role.Permissions) == 0 {
			t.Fatalf("role %q has no permissions", role.Name)
		}
		for _, perm := range role.Permissions {
			if !strings.Contains(perm, ":") {
				t.Fatalf("permission %q is not verb:resource shaped", perm)
			}
		}
	}
	for _, want := range []string{RoleAdmin, RoleOperator, RoleViewer} {
		if !names[want] {
			t.Fatalf("expected the role %q", want)
		}
	}
}
