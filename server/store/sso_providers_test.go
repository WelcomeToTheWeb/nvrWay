package store

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"nvr/server/models"
)

func provider(name string) *models.SSOProvider {
	return &models.SSOProvider{
		Name:         name,
		Type:         models.SSOTypeOIDC,
		ClientID:     "nvrdev",
		ClientSecret: "sealed-secret",
		Issuer:       "https://id.example/realms/" + name,
		DiscoveryURL: "https://id.example/realms/" + name + "/.well-known/openid-configuration",
		Enabled:      true,
	}
}

func TestSSOProviderCreateAndSlugLookup(t *testing.T) {
	st := newTestStore(t)

	p := provider("keycloak")
	if err := st.SSOProviders.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.ID == "" {
		t.Fatal("expected the store to mint the provider id")
	}
	if p.Type != models.SSOTypeOIDC {
		t.Fatalf("v1 speaks OIDC only, got %q", p.Type)
	}
	// The link policy is the safe default on a provider row, filled by the
	// store, not left to each caller to remember.
	if p.LinkPolicy != models.SSOLinkPolicyPending {
		t.Fatalf("expected the pending default, got %q", p.LinkPolicy)
	}

	// The slug is the string in /api/auth/sso/:provider/authorize.
	byName, err := st.SSOProviders.GetByName("keycloak")
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	if byName.Issuer != p.Issuer {
		t.Fatalf("expected the provider back, got %+v", byName)
	}
	if _, err := st.SSOProviders.GetByName("nope"); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("expected the named not-found, got %v", err)
	}

	dup := provider("keycloak")
	if err := st.SSOProviders.Create(dup); err == nil {
		t.Fatal("expected the unique slug to be refused: a console with two buttons naming one provider is ambiguous")
	}
}

func TestSSOProviderSavedDisabledStaysDisabled(t *testing.T) {
	st := newTestStore(t)

	p := provider("staged-idp")
	p.Enabled = false
	if err := st.SSOProviders.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	loaded, err := st.SSOProviders.GetByName("staged-idp")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Enabled {
		t.Fatal("a provider saved disabled arrived enabled (schema default won)")
	}

	// A provider whose IdP does not assert email_verified: the strict gate
	// would lock everyone out, so the operator turns it off, and off must land.
	loose := provider("no-verify")
	loose.RequireVerifiedEmail = false
	if err := st.SSOProviders.Create(loose); err != nil {
		t.Fatalf("create: %v", err)
	}
	loaded, err = st.SSOProviders.GetByName("no-verify")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.RequireVerifiedEmail {
		t.Fatal("require_verified_email=true arrived from the schema default")
	}
}

func TestSSOProviderListEnabledIsTheLoginPage(t *testing.T) {
	st := newTestStore(t)

	corp := provider("corp")
	corp.SortOrder = 2
	google := provider("google")
	google.SortOrder = 1
	staged := provider("staged")
	staged.Enabled = false
	staged.SortOrder = 0
	for _, p := range []*models.SSOProvider{corp, google, staged} {
		if err := st.SSOProviders.Create(p); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	enabled, err := st.SSOProviders.ListEnabled()
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	if len(enabled) != 2 {
		t.Fatalf("the login page lists enabled providers only, got %d", len(enabled))
	}
	if enabled[0].Name != "google" || enabled[1].Name != "corp" {
		t.Fatalf("expected the operator's sort order, got %v, %v", enabled[0].Name, enabled[1].Name)
	}

	// The settings page shows the disabled one: "why is there no login button"
	// is a question about a row that is present and switched off.
	all, err := st.SSOProviders.ListAll()
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected every provider, got %d", len(all))
	}
}

func TestSSOProviderUpdateKeepsTheClientSecretItWasNotGiven(t *testing.T) {
	st := newTestStore(t)
	p := provider("keycloak")
	if err := st.SSOProviders.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	sealed := p.ClientSecret

	// Editing the issuer from a console that never read the secret must not
	// erase it: the login would fail at the token endpoint, far from the edit.
	edit := provider("keycloak")
	edit.ID = p.ID
	edit.Issuer = "https://id.example/realms/renamed"
	edit.ClientSecret = ""
	if err := st.SSOProviders.Update(edit, false); err != nil {
		t.Fatalf("update: %v", err)
	}

	loaded, err := st.SSOProviders.GetByName("keycloak")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Issuer != "https://id.example/realms/renamed" {
		t.Fatalf("expected the edit to land, got %q", loaded.Issuer)
	}
	if loaded.ClientSecret != sealed {
		t.Fatal("the client secret was wiped by an edit that did not carry it")
	}

	// A write that does carry it replaces it.
	rotate := provider("keycloak")
	rotate.ID = p.ID
	rotate.ClientSecret = "sealed-secret-2"
	if err := st.SSOProviders.Update(rotate, true); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	loaded, err = st.SSOProviders.GetByName("keycloak")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.ClientSecret != "sealed-secret-2" {
		t.Fatal("expected the rotated secret")
	}

	if err := st.SSOProviders.Update(&models.SSOProvider{ID: "missing"}, false); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestSSOProviderDiscoveryStateIsStored(t *testing.T) {
	st := newTestStore(t)
	p := provider("keycloak")
	if err := st.SSOProviders.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The console draws a named state, not a spinner, and "which IdP is
	// unhealthy" is a question the row answers.
	at := time.Now().UTC()
	if err := st.SSOProviders.MarkDiscovery(p.ID, "no_code_flow", `{"code_grant":{}}`, at, false); err != nil {
		t.Fatalf("mark discovery: %v", err)
	}
	loaded, err := st.SSOProviders.GetByName("keycloak")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.DiscoveryState != "no_code_flow" {
		t.Fatalf("expected the named discovery state, got %q", loaded.DiscoveryState)
	}
	if loaded.DiscoveredAt == nil {
		t.Fatal("expected the discovery timestamp")
	}
	if loaded.PKCESupported {
		t.Fatal("discovery said no PKCE; the authorize redirect must not send a challenge")
	}
	if loaded.DiscoveryDoc == "" {
		t.Fatal("the document as published is what the preview shows")
	}

	if err := st.SSOProviders.MarkDiscovery(p.ID, "ok", `{"code_grant":{}}`, at, true); err != nil {
		t.Fatalf("mark discovery: %v", err)
	}
	loaded, err = st.SSOProviders.GetByName("keycloak")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !loaded.PKCESupported {
		t.Fatal("expected PKCE support recorded from discovery")
	}
}

func TestSSOProviderDeleteLeavesThePeople(t *testing.T) {
	st := newTestStore(t)
	p := provider("keycloak")
	if err := st.SSOProviders.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	account := &models.User{
		Username: "ada@id.example", Email: "ada@id.example",
		Provider: "keycloak", ProviderID: "00u1abc", Status: models.UserStatusActive,
	}
	if err := st.Users.Create(account); err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := st.SSOProviders.Delete(p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.SSOProviders.GetByName("keycloak"); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("expected the provider gone, got %v", err)
	}

	// A provider is a door, not the people behind it: deleting a door does not
	// delete them, and deleting a door must not log anyone out of an account
	// they also reach another way.
	if _, err := st.Users.GetByID(account.ID); err != nil {
		t.Fatalf("the account the provider provisioned must survive: %v", err)
	}

	if err := st.SSOProviders.Delete("missing"); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestPendingIdentityQueue(t *testing.T) {
	st := newTestStore(t)

	existing := newUser("ada")
	existing.Email = "ada@id.example"
	if err := st.Users.Create(existing); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// The assertion the installation refused to act on: an asserted email that
	// belongs to an account that already exists.
	pending := &models.PendingIdentity{
		Provider:        "keycloak",
		ProviderSubject: "00u1abc",
		Email:           "ada@id.example",
		Username:        "ada-kc",
		Claims:          `{"email":"ada@id.example","groups":["nvr-ops"]}`,
		MatchedUserID:   existing.ID,
	}
	if err := st.PendingIdentities.Create(pending); err != nil {
		t.Fatalf("create: %v", err)
	}
	if pending.Status != models.IdentityStatusPending {
		t.Fatalf("a parked identity is pending, got %q", pending.Status)
	}
	if pending.Claims == "" {
		t.Fatal("the assertion is the record an admin decides with; the IdP's response is not reproducible later")
	}

	list, err := st.PendingIdentities.ListPending()
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 queued identity, got %d", len(list))
	}

	// A decision, with the admin recorded as a username as well as an id:
	// accounts are deleted, and a record that resolves to "user 9f3… decided
	// this" is not a record.
	if err := st.PendingIdentities.MarkDecided(pending.ID, models.IdentityStatusApproved,
		"admin-id", "admin", "operator approved it as its own account at the viewer floor", ""); err != nil {
		t.Fatalf("mark decided: %v", err)
	}
	loaded, err := st.PendingIdentities.GetByID(pending.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Status != models.IdentityStatusApproved || loaded.DecidedAt == nil || loaded.DecidedByUsername != "admin" {
		t.Fatalf("expected the decision recorded, got %+v", loaded)
	}
	if list, err := st.PendingIdentities.ListPending(); err != nil || len(list) != 0 {
		t.Fatalf("a decided identity leaves the queue, got %d (%v)", len(list), err)
	}
	if all, err := st.PendingIdentities.ListAll(); err != nil || len(all) != 1 {
		t.Fatalf("the record of the decision stays, got %d (%v)", len(all), err)
	}

	// A status no code path reads is a queue entry that looks decided and is
	// not, so the decision values are validated at the write.
	if err := st.PendingIdentities.MarkDecided(pending.ID, "probably-linked", "id", "admin", "", ""); err == nil {
		t.Fatal("expected an unknown decision to be refused")
	}

	if _, err := st.PendingIdentities.GetByID("missing"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}
}
