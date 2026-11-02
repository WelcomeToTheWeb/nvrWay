package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"nvr/server/models"
	"nvr/server/sso"
	"nvr/server/sso/testidp/provider"
)

// The OIDC surface, walked end to end against the in-repo test IdP (S2-05): the
// redirect out, the consent walk, the callback back, the fragment handoff, and the
// exchange the console makes with what it caught. No `docker compose up`, no
// `webhook.site`, no operator machine.
//
// The reason these tests exist at the API layer, beside `sso/`'s own suites: the
// contract lives in `sso/oauth.go`, and the *shape* — the state record, the
// redirect URI, the fragment, the ledger rows, the queue an admin works through —
// is here. A contract tested only in its own package can be wired to the router
// wrong.

const (
	ssoClientID     = "nvrdev"
	ssoClientSecret = "devsecret"
	ssoProviderName = "testidp"
)

type ssoFixture struct {
	h      *Handlers
	server *httptest.Server
	idp    *provider.IdP
	// client is the browser: it does not follow redirects, because a walk that
	// follows them cannot see the state, the code, or the fragment.
	client   *http.Client
	provider *models.SSOProvider
}

// newSSOFixture starts the IdP, discovers it the way the product does, and mounts
// the API on a local server. `mutate` is how a test changes the contract before
// the login starts — the domain allow-list, the link policy, the switch.
func newSSOFixture(t *testing.T, claims provider.Claims, mutate func(*models.SSOProvider)) *ssoFixture {
	t.Helper()

	st := newStore(t)
	idp := provider.New(provider.Options{
		ClientID:     ssoClientID,
		ClientSecret: ssoClientSecret,
		Claims:       claims,
	})
	t.Cleanup(idp.Close)

	doc, err := sso.Discover(sso.DefaultHTTPClient(), idp.Issuer(), "")
	if err != nil {
		t.Fatalf("discovery against the in-repo IdP: %v", err)
	}

	row := &models.SSOProvider{
		Name:           ssoProviderName,
		Type:           models.SSOTypeOIDC,
		Issuer:         idp.Issuer(),
		ClientID:       ssoClientID,
		ClientSecret:   ssoClientSecret,
		Scope:          "openid email profile",
		AuthURL:        doc.AuthorizationEndpoint,
		TokenURL:       doc.TokenEndpoint,
		UserInfoURL:    doc.UserInfoEndpoint,
		JwksURI:        doc.JwksURI,
		Enabled:        true,
		AllowedDomains: "nvrway.test",
		// The fixture asserts `email_verified: true`; the strict gate is the
		// product's default, so the walk runs under it.
		RequireVerifiedEmail: true,
		LinkPolicy:           models.SSOLinkPolicyPending,
		PKCESupported:        doc.PKCESupported(),
	}
	if mutate != nil {
		mutate(row)
	}
	if err := st.SSOProviders.Create(row); err != nil {
		t.Fatalf("saving the provider row: %v", err)
	}

	h := newHandlers(t, st)

	server := httptest.NewServer(routerFor(t, h))
	t.Cleanup(server.Close)

	// WEB_URL is the fixture's own origin. That is the dev compose's shape: one
	// origin serves the console and proxies `/api` to the server, so the IdP's
	// redirect back to `${WEB_URL}/api/auth/sso/<provider>/callback` lands on the
	// same handler that started the login — which is exactly the string the
	// operator whitelists, and the walk is drivable.
	h.Cfg.WebURL = server.URL

	return &ssoFixture{
		h: h,
		// The provider row is re-read from the store so a test sees what the
		// handler sees, including what discovery wrote.
		provider: row,
		server:   server,
		idp:      idp,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (f *ssoFixture) get(t *testing.T, target string) (int, string) {
	t.Helper()
	resp, err := f.client.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

// walk drives the browser from our authorize route to the console's landing URL.
// It answers the handoff Location and the three statuses it took to get there.
func (f *ssoFixture) walk(t *testing.T) (string, []int) {
	t.Helper()

	status, authorize := f.get(t, f.server.URL+"/api/auth/sso/"+ssoProviderName+"/authorize")
	if status != http.StatusFound {
		t.Fatalf("authorize answered %d, want 302 to the IdP", status)
	}

	status, callback := f.get(t, authorize)
	if status != http.StatusFound {
		t.Fatalf("the IdP's authorize answered %d, want 302 back with a code", status)
	}

	status, handoff := f.get(t, callback)
	if status != http.StatusFound {
		t.Fatalf("our callback answered %d, want 302 to the console with the fragment", status)
	}
	return handoff, []int{status}
}

// handoffParts parses the fragment the console reads.
//
// The parsing is deliberate: `url.ParseQuery` refuses a query containing a
// semicolon, and an operator's own IdP message — the text we hand back verbatim in
// `error_description` — is full of them. A console reads this with
// `URLSearchParams`, so the suite reads it the same way: split on the separators
// that are ours, and unescape the values.
func handoffParts(t *testing.T, handoff string) (base string, fragment map[string]string) {
	t.Helper()
	parsed, err := url.Parse(handoff)
	if err != nil {
		t.Fatalf("the handoff is not a URL: %v", err)
	}
	fragment = map[string]string{}
	for _, pair := range strings.Split(parsed.Fragment, "&") {
		if pair == "" {
			continue
		}
		key, value, _ := strings.Cut(pair, "=")
		decoded, err := url.QueryUnescape(value)
		if err != nil {
			t.Fatalf("the fragment value %q is not escapable: %v", value, err)
		}
		fragment[key] = decoded
	}
	return parsed.Path, fragment
}

func ssoClaims() provider.Claims {
	return provider.Claims{
		Subject:       "subject-ops",
		Email:         "ops@nvrway.test",
		EmailVerified: true,
		Name:          "Ops Operator",
		Username:      "ops",
		Groups:        []string{"viewers"},
	}
}

func TestSSOHandoffWalksAuthorizeCallbackExchange(t *testing.T) {
	f := newSSOFixture(t, ssoClaims(), nil)

	handoff, _ := f.walk(t)
	base, fragment := handoffParts(t, handoff)

	if base != "/auth/callback" {
		t.Errorf("the handoff landed on %q, want the console's /auth/callback", base)
	}
	if !strings.HasPrefix(handoff, f.server.URL+"/auth/callback#") {
		t.Fatalf("the handoff is %q, want it to start %s/auth/callback#", handoff, f.server.URL)
	}
	access, refresh := fragment["access_token"], fragment["refresh_token"]
	if access == "" || refresh == "" {
		t.Fatalf("the fragment carries no token pair: %q", handoff)
	}
	if strings.Contains(handoff, "?") {
		t.Errorf("the handoff carries a query: %q — the tokens travel in the fragment, which no server ever receives", handoff)
	}

	// The identity is an account now, provisioned at the viewer floor.
	user, err := f.h.St.Users.FindByProvider(ssoProviderName, "subject-ops")
	if err != nil || user == nil {
		t.Fatalf("the login provisioned no account: %v", err)
	}
	if user.Username != "ops" {
		t.Errorf("username = %q, want the provider's username claim", user.Username)
	}
	roles, err := f.h.St.Users.RolesFor(user.ID)
	if err != nil {
		t.Fatalf("reading roles: %v", err)
	}
	if len(roles) != 1 || roles[0] != models.RoleViewer {
		t.Fatalf("JIT roles = %v, want exactly [viewer]: the floor the plan sets", roles)
	}

	// The handoff's access token is the credential the console carries.
	status, profile, _ := do(t, routerFor(t, f.h), request{
		method: http.MethodGet, path: "/api/auth/profile", bearer: access,
	})
	if status != http.StatusOK {
		t.Fatalf("the handoff's access token did not work on the API: %d %v", status, profile)
	}

	// The refresh token is bound to a session row, so it can be rotated and
	// revoked. A handoff that issues a refresh token with no row is a credential
	// nothing can revoke.
	session, err := f.h.St.Sessions.GetByTokenHash(models.HashRefreshToken(refresh))
	if err != nil || session == nil {
		t.Fatalf("the handoff issued a refresh token with no session row: %v", err)
	}
	if session.UserID != user.ID {
		t.Errorf("the session belongs to %q, want the account the identity became", session.UserID)
	}

	rows := auditRows(t, f.h.St, models.AuditSSOLogin)
	if len(rows) != 1 {
		t.Fatalf("the SSO login wrote %d ledger rows, want 1", len(rows))
	}
	row := rows[0]
	if row.Outcome != models.AuditOutcomeOK || row.ActorUsername != user.Username {
		t.Errorf("the login row is %+v, want an ok row naming the account", row)
	}
	for _, want := range []string{`"subject":"subject-ops"`, `"email":"ops@nvrway.test"`, `"created_jit":true`, `"verified_by_id_token":true`} {
		if !strings.Contains(row.Details, want) {
			t.Errorf("the login row's details are missing %s: %s", want, row.Details)
		}
	}
}

func TestSSOAuthorizeSendsStateNonceAndS256Challenge(t *testing.T) {
	f := newSSOFixture(t, ssoClaims(), nil)

	_, authorize := f.get(t, f.server.URL+"/api/auth/sso/"+ssoProviderName+"/authorize")
	target, err := url.Parse(authorize)
	if err != nil {
		t.Fatalf("the authorize redirect is not a URL: %v", err)
	}
	q := target.Query()
	if q.Get("client_id") != ssoClientID || q.Get("response_type") != "code" {
		t.Errorf("the redirect to the IdP is %v, want our client and the code flow", q)
	}
	if q.Get("redirect_uri") != f.server.URL+"/api/auth/sso/"+ssoProviderName+"/callback" {
		t.Errorf("redirect_uri = %q, want exactly the callback route", q.Get("redirect_uri"))
	}
	if q.Get("state") == "" || q.Get("nonce") == "" {
		t.Fatalf("the redirect carries no state/nonce: %v", q)
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Errorf("the redirect carries %q/%q, want an S256 challenge", q.Get("code_challenge_method"), q.Get("code_challenge"))
	}
	if strings.Contains(authorize, ssoClientSecret) {
		t.Error("the client secret travelled in the browser's URL")
	}

	// The state record is the one thing that makes the callback a login rather
	// than a request anyone can send.
	if got := f.h.states.Len(); got != 1 {
		t.Errorf("logins in flight = %d, want 1", got)
	}
}

func TestSSOStateIsShortLivedAndSingleUse(t *testing.T) {
	f := newSSOFixture(t, ssoClaims(), nil)

	// A callback carrying a state we never issued. Nobody started this login: the
	// refusal is state_mismatch, and the ledger records an unrequested callback.
	neverIssued, err := randomToken()
	if err != nil {
		t.Fatalf("making a state value nobody issued: %v", err)
	}
	callback := f.server.URL + "/api/auth/sso/" + ssoProviderName + "/callback?code=code-" + neverIssued + "&state=" + neverIssued
	status, location := f.get(t, callback)
	if status != http.StatusFound {
		t.Fatalf("an unrequested callback answered %d, want 302 to the console's failure state", status)
	}
	if !strings.Contains(location, "error="+sso.ReasonStateMismatch) {
		t.Errorf("the refusal is %q, want the reason %s", location, sso.ReasonStateMismatch)
	}
	if rows := auditRows(t, f.h.St, models.AuditSSOLoginRefused); len(rows) != 1 {
		t.Fatalf("the unrequested callback wrote %d ledger rows, want 1", len(rows))
	}

	// A completed login consumes its state: the walk, then the same callback
	// again. The code is single use at the IdP, and the state is single use here —
	// a replay must not be able to mint a second session.
	_, authorize := f.get(t, f.server.URL+"/api/auth/sso/"+ssoProviderName+"/authorize")
	parsed, _ := url.Parse(authorize)
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatal("the authorize redirect carries no state to replay")
	}
	_, callbackURL := f.get(t, authorize)
	_, handoff := f.get(t, callbackURL)
	if !strings.Contains(handoff, "access_token=") {
		t.Fatalf("the walk did not hand off tokens: %q", handoff)
	}
	if left := f.h.states.Len(); left != 0 {
		t.Errorf("logins in flight after a completed login = %d, want 0: the state is consumed by the callback", left)
	}
	status, location = f.get(t, callbackURL)
	if status != http.StatusFound || !strings.Contains(location, "error="+sso.ReasonStateMismatch) {
		t.Fatalf("the replay answered %d %q, want the state_mismatch refusal", status, location)
	}

	// Short-lived: five minutes, by clock, not by sleep.
	fresh := newSSOFixture(t, ssoClaims(), nil)
	_, authorize = fresh.get(t, fresh.server.URL+"/api/auth/sso/"+ssoProviderName+"/authorize")
	parsed, _ = url.Parse(authorize)
	state = parsed.Query().Get("state")
	_, lateCallback := fresh.get(t, authorize)

	fresh.h.SetClock(func() time.Time { return time.Now().UTC().Add(LoginStateTTL + time.Minute) })
	status, location = fresh.get(t, lateCallback)
	if status != http.StatusFound || !strings.Contains(location, "error="+sso.ReasonStateExpired) {
		t.Fatalf("a callback after the state expired answered %d %q, want the state_expired refusal", status, location)
	}
	if left := fresh.h.states.Len(); left != 0 {
		t.Errorf("the expired state is still in memory (%d), want it consumed by the refusal", left)
	}
}

func TestSSOExchangeAnswersProfileAndRoles(t *testing.T) {
	f := newSSOFixture(t, ssoClaims(), nil)
	handoff, _ := f.walk(t)
	_, fragment := handoffParts(t, handoff)
	access := fragment["access_token"]

	router := routerFor(t, f.h)
	status, body, _ := do(t, router, request{method: http.MethodPost, path: "/api/auth/sso/exchange", bearer: access})
	if status != http.StatusOK {
		t.Fatalf("exchange: got %d %v, want 200", status, body)
	}
	if body["username"] != "ops" || body["provider"] != ssoProviderName {
		t.Errorf("exchange answered %+v, want the account the token names, with its provider", body)
	}
	roles, _ := body["roles"].([]any)
	if len(roles) != 1 || roles[0] != models.RoleViewer {
		t.Errorf("exchange roles = %v, want [viewer]", body["roles"])
	}
	if _, has := body["password_hash"]; has {
		t.Error("the exchange carries a password hash")
	}

	// The exchange is the console's check that the token it caught is its own.
	// An access token minted for an account that is not the handoff's answers the
	// account it names, and nothing else.
	watcher := newUser(t, f.h.St, "watcher-from-the-form", models.RoleViewer)
	other := accessToken(t, f.h.St, f.h.JWT, watcher)
	status, body, _ = do(t, router, request{method: http.MethodPost, path: "/api/auth/sso/exchange", bearer: other})
	if status != http.StatusOK || body["username"] != "watcher-from-the-form" {
		t.Fatalf("exchange with another account's token answered %d %v", status, body)
	}

	status, body, _ = do(t, router, request{method: http.MethodPost, path: "/api/auth/sso/exchange"})
	if status != http.StatusUnauthorized {
		t.Fatalf("exchange with no bearer answered %d %v, want 401", status, body)
	}
}

func TestSSOProviderSwitchIsHonouredAtTheCallback(t *testing.T) {
	f := newSSOFixture(t, ssoClaims(), nil)

	_, authorize := f.get(t, f.server.URL+"/api/auth/sso/"+ssoProviderName+"/authorize")
	_, callbackURL := f.get(t, authorize)

	row, err := f.h.St.SSOProviders.GetByName(ssoProviderName)
	if err != nil {
		t.Fatalf("reading the provider row: %v", err)
	}
	row.Enabled = false
	if err := f.h.St.SSOProviders.Update(row, false); err != nil {
		t.Fatalf("switching the provider off: %v", err)
	}

	status, location := f.get(t, callbackURL)
	if status == http.StatusFound && strings.Contains(location, "access_token=") {
		t.Fatal("a switched-off provider completed a login")
	}
	if strings.Contains(location, sso.ReasonProviderDisabled) {
		return
	}
	if status != http.StatusForbidden {
		t.Fatalf("the callback to a switched-off provider answered %d %q, want 403 or the named refusal", status, location)
	}
	if rows := auditRows(t, f.h.St, models.AuditSSOLoginRefused); len(rows) == 0 {
		t.Error("a login refused by the provider switch is not in the ledger")
	}
}

func TestSSOContractRefusalArrivesAsItsOwnReason(t *testing.T) {
	// An IdP that asserts an address outside the operator's allow-list is the
	// plan's attack: the login must end in a named refusal and a ledger row
	// naming who tried to arrive as whom.
	f := newSSOFixture(t, provider.Claims{
		Subject: "subject-outsider", Email: "someone@else.example", EmailVerified: true,
		Name: "Outsider", Username: "outsider",
	}, nil)

	handoff, _ := f.walk(t)
	base, fragment := handoffParts(t, handoff)
	if base != "/auth/callback" {
		t.Fatalf("the refusal landed on %q, want the console's failure state", base)
	}
	if got := fragment["error"]; got != sso.ReasonDomainNotAllowed {
		t.Fatalf("the refusal is %q, want %s", got, sso.ReasonDomainNotAllowed)
	}
	if strings.Contains(handoff, "access_token=") {
		t.Fatal("the refusal handed off session tokens")
	}

	rows := auditRows(t, f.h.St, models.AuditSSOLoginRefused)
	if len(rows) != 1 {
		t.Fatalf("the refusal wrote %d ledger rows, want 1", len(rows))
	}
	for _, want := range []string{sso.ReasonDomainNotAllowed, "subject-outsider", "someone@else.example"} {
		if !strings.Contains(rows[0].Details, want) {
			t.Errorf("the refusal row is missing %q: %s", want, rows[0].Details)
		}
	}

	// The attack is refused *and* unprovisioned: no account appeared for it.
	if _, err := f.h.St.Users.FindByProvider(ssoProviderName, "subject-outsider"); err == nil {
		t.Error("the refused assertion provisioned an account")
	}
}

func TestSSOProviderListCarriesTheRedirectURITheOperatorWhitelists(t *testing.T) {
	f := newSSOFixture(t, ssoClaims(), nil)

	status, body, _ := do(t, routerFor(t, f.h), request{method: http.MethodGet, path: "/api/auth/sso/providers"})
	if status != http.StatusOK {
		t.Fatalf("the provider list answered %d %v", status, body)
	}
	list, _ := body["providers"].([]any)
	if len(list) != 1 {
		t.Fatalf("the list is %v, want the one enabled provider", body["providers"])
	}
	row := list[0].(map[string]any)
	want := f.server.URL + "/api/auth/sso/" + ssoProviderName + "/callback"
	if row["redirect_uri"] != want {
		t.Errorf("redirect_uri = %q, want the exact string the operator pastes into the IdP: %s", row["redirect_uri"], want)
	}
	if row["provider"] != ssoProviderName {
		t.Errorf("provider = %v, want the slug that appears in the route", row["provider"])
	}
}

func TestSSOProviderWithAMismatchedRedirectURITellsTheOperatorTheString(t *testing.T) {
	f := newSSOFixture(t, ssoClaims(), func(p *models.SSOProvider) {
		// The row carries what discovery saw, and the IdP was configured for a
		// different host than WEB_URL. This is the state where every login fails at
		// the IdP with `invalid_redirect_uri`, and the operator has to be told the
		// string, not the symptom.
		p.RedirectURI = "http://somewhere.else/api/auth/sso/" + ssoProviderName + "/callback"
	})

	status, body, _ := do(t, routerFor(t, f.h), request{
		method: http.MethodGet, path: "/api/auth/sso/" + ssoProviderName + "/authorize",
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("authorize answered %d %v, want 422 naming the redirect URI", status, body)
	}
	want := f.server.URL + "/api/auth/sso/" + ssoProviderName + "/callback"
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, want) {
		t.Errorf("the answer is %q, want it to contain the exact string %s", msg, want)
	}
}

// --- The pending-identity queue -------------------------------------------------

// queueIdentity parks an assertion the contract refused: the asserted email
// belongs to an existing account, and `link_policy=pending` is the default.
func (f *ssoFixture) queueIdentity(t *testing.T, subject, email, matchedUserID string) *models.PendingIdentity {
	t.Helper()
	row := &models.PendingIdentity{
		Provider:        ssoProviderName,
		ProviderSubject: subject,
		Email:           email,
		Username:        strings.SplitN(email, "@", 2)[0],
		DisplayName:     "Asserted Person",
		Claims:          `{"sub":"` + subject + `","email":"` + email + `"}`,
		MatchedUserID:   matchedUserID,
		Status:          models.IdentityStatusPending,
	}
	if err := f.h.St.PendingIdentities.Create(row); err != nil {
		t.Fatalf("parking the identity: %v", err)
	}
	return row
}

func TestSSOIdentitiesQueueIsAdminOnlyAndReadable(t *testing.T) {
	f := newSSOFixture(t, ssoClaims(), nil)
	router := routerFor(t, f.h)

	admin, err := f.h.St.Users.GetByUsername("admin")
	if err != nil {
		t.Fatalf("reading the break-glass admin: %v", err)
	}
	f.queueIdentity(t, "subject-pending", admin.Email, admin.ID)

	// The queue is an administrator's surface: a viewer and an operator are refused
	// by the same gate, and each refusal is a ledger row.
	watcher := newUser(t, f.h.St, "watcher-queue", models.RoleViewer)
	worker := newUser(t, f.h.St, "operator-queue", models.RoleOperator)
	for _, u := range []*models.User{watcher, worker} {
		status, body, _ := do(t, router, request{method: http.MethodGet, path: "/api/identities", bearer: accessToken(t, f.h.St, f.h.JWT, u)})
		if status != http.StatusForbidden {
			t.Fatalf("%s reading the queue: got %d %v, want 403", u.Username, status, body)
		}
	}

	status, body, _ := do(t, router, request{method: http.MethodGet, path: "/api/identities", bearer: accessToken(t, f.h.St, f.h.JWT, admin)})
	if status != http.StatusOK {
		t.Fatalf("admin reading the queue: got %d %v, want 200", status, body)
	}
	list, _ := body["identities"].([]any)
	if len(list) != 1 {
		t.Fatalf("the queue is %v, want the one parked assertion", body["identities"])
	}
	row := list[0].(map[string]any)
	if row["email"] != admin.Email || row["matched_user_id"] != admin.ID {
		t.Errorf("the queue row is %+v, want the assertion and the account it points at", row)
	}
	if row["claims"] == nil {
		t.Error("the queue row hides the claims the admin decides with")
	}
	// The queue read is on the record, and so are the two refusals — under the
	// same verb, which is the point: an operator hunting "who touched the identity
	// queue" gets one action to filter on.
	var readOK, readRefused int
	for _, row := range auditRows(t, f.h.St, models.AuditIdentityRead) {
		switch row.Outcome {
		case models.AuditOutcomeOK:
			readOK++
		case models.AuditOutcomeRefused:
			readRefused++
		}
	}
	if readOK != 1 {
		t.Errorf("queue reads recorded %d ok rows, want 1", readOK)
	}
	if readRefused != 2 {
		t.Errorf("queue reads recorded %d refused rows, want 2: the viewer and the operator are on the record", readRefused)
	}
}

func TestSSOIdentitiesApproveCreatesTheViewerFloorAccount(t *testing.T) {
	f := newSSOFixture(t, ssoClaims(), nil)
	router := routerFor(t, f.h)
	admin, _ := f.h.St.Users.GetByUsername("admin")
	row := f.queueIdentity(t, "subject-newcomer", "newcomer@nvrway.test", "")
	token := accessToken(t, f.h.St, f.h.JWT, admin)

	status, body, _ := do(t, router, request{
		method: http.MethodPost, path: "/api/identities/" + row.ID + "/approve", bearer: token,
	})
	if status != http.StatusOK {
		t.Fatalf("approve: got %d %v, want 200", status, body)
	}

	user, err := f.h.St.Users.FindByProvider(ssoProviderName, "subject-newcomer")
	if err != nil || user == nil {
		t.Fatalf("the approval created no account: %v", err)
	}
	roles, _ := f.h.St.Users.RolesFor(user.ID)
	if len(roles) != 1 || roles[0] != models.RoleViewer {
		t.Fatalf("approved roles = %v, want exactly [viewer]", roles)
	}

	decided, err := f.h.St.PendingIdentities.GetByID(row.ID)
	if err != nil {
		t.Fatalf("reading the decided row: %v", err)
	}
	if decided.Status != models.IdentityStatusApproved || decided.CreatedUserID != user.ID {
		t.Errorf("the decision row is %+v, want approved and naming the account it created", decided)
	}
	if decided.DecidedByUsername != "admin" {
		t.Errorf("the decision names %q as the decider, want the admin who made it", decided.DecidedByUsername)
	}

	if rows := auditRows(t, f.h.St, models.AuditIdentityApprove); len(rows) != 1 {
		t.Fatalf("the approval wrote %d ledger rows, want 1", len(rows))
	}

	// A second decision on a decided row is refused: the queue is a record of
	// decisions, not a form to submit twice.
	status, body, _ = do(t, router, request{
		method: http.MethodPost, path: "/api/identities/" + row.ID + "/approve", bearer: token,
	})
	if status != http.StatusConflict {
		t.Fatalf("approving a decided identity: got %d %v, want 409", status, body)
	}
}

func TestSSOIdentitiesLinkIsAuditedAndStopsTheOldTokens(t *testing.T) {
	f := newSSOFixture(t, ssoClaims(), nil)
	router := routerFor(t, f.h)
	admin, _ := f.h.St.Users.GetByUsername("admin")

	// The shape the contract refuses: an assertion whose email belongs to the
	// break-glass admin. The admin's password login is the door that stays open —
	// an installation cannot lock itself out of the break-glass account.
	row := f.queueIdentity(t, "subject-admin-asserted", admin.Email, admin.ID)
	token := accessToken(t, f.h.St, f.h.JWT, admin)
	refresh := refreshToken(t, f.h.JWT, admin)
	recordSession(t, f.h.St, admin.ID, refresh)

	status, body, _ := do(t, router, request{
		method: http.MethodPost, path: "/api/identities/" + row.ID + "/link",
		bearer: token, body: map[string]any{"user_id": admin.ID},
	})
	if status != http.StatusOK {
		t.Fatalf("link: got %d %v, want 200", status, body)
	}

	linked, err := f.h.St.Users.GetByID(admin.ID)
	if err != nil {
		t.Fatalf("reading the linked account: %v", err)
	}
	if linked.Provider != ssoProviderName || linked.ProviderID != "subject-admin-asserted" {
		t.Fatalf("the account carries %s/%q, want the provider identity it was linked to", linked.Provider, linked.ProviderID)
	}
	if linked.PasswordHash != "" {
		t.Error("a linked account keeps its password hash: an account that signs in through a provider must not have a second door")
	}

	// The roles are untouched: merging an identity is not a way to grant authority.
	roles, _ := f.h.St.Users.RolesFor(admin.ID)
	if len(roles) != 1 || roles[0] != models.RoleAdmin {
		t.Errorf("roles after the link = %v, want the account's own roles", roles)
	}

	// Every credential issued before the link stops working, because the account
	// it names is now a different kind of account.
	if dropped, _ := body["live_sessions_dropped"].(float64); dropped < 1 {
		t.Errorf("live_sessions_dropped = %v, want the sessions the account held", body["live_sessions_dropped"])
	}
	status, _, _ = do(t, router, request{method: http.MethodGet, path: "/api/auth/profile", bearer: token})
	if status != http.StatusUnauthorized {
		t.Fatalf("the pre-link access token still verifies: got %d, want 401", status)
	}
	status, _, _ = do(t, router, request{
		method: http.MethodPost, path: "/api/auth/refresh",
		body: map[string]any{"refresh_token": refresh},
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("the pre-link refresh token still rotates: got %d, want 401", status)
	}

	rows := auditRows(t, f.h.St, models.AuditIdentityLink)
	if len(rows) != 1 {
		t.Fatalf("the link wrote %d ledger rows, want 1", len(rows))
	}
	if !strings.Contains(rows[0].Details, "subject-admin-asserted") {
		t.Errorf("the link row is missing the assertion it acted on: %s", rows[0].Details)
	}
	if rows[0].TargetID != admin.ID {
		t.Errorf("the link row points at %q, want the account the identity was merged into", rows[0].TargetID)
	}
	if rows[0].ActorUsername != "admin" {
		t.Errorf("the link row names %q as the actor, want the admin who decided", rows[0].ActorUsername)
	}

	// And the queue records the decision against the account it created.
	decided, _ := f.h.St.PendingIdentities.GetByID(row.ID)
	if decided.Status != models.IdentityStatusLinked || decided.CreatedUserID != admin.ID {
		t.Errorf("the decision row is %+v, want linked and naming the account", decided)
	}
}

func TestSSOIdentitiesRefuseNeedsItsReason(t *testing.T) {
	f := newSSOFixture(t, ssoClaims(), nil)
	router := routerFor(t, f.h)
	admin, _ := f.h.St.Users.GetByUsername("admin")
	row := f.queueIdentity(t, "subject-nobody", "nobody@nvrway.test", "")
	token := accessToken(t, f.h.St, f.h.JWT, admin)

	status, body, _ := do(t, router, request{
		method: http.MethodPost, path: "/api/identities/" + row.ID + "/refuse",
		bearer: token, body: map[string]any{},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("refusing without a reason: got %d %v, want 400", status, body)
	}

	status, body, _ = do(t, router, request{
		method: http.MethodPost, path: "/api/identities/" + row.ID + "/refuse",
		bearer: token, body: map[string]any{"reason": "this address is not ours"},
	})
	if status != http.StatusOK {
		t.Fatalf("refuse: got %d %v, want 200", status, body)
	}
	decided, _ := f.h.St.PendingIdentities.GetByID(row.ID)
	if decided.Status != models.IdentityStatusRefused || decided.Reason != "this address is not ours" {
		t.Errorf("the decision row is %+v, want refused with the reason", decided)
	}
	if rows := auditRows(t, f.h.St, models.AuditIdentityRefuse); len(rows) != 1 {
		t.Fatalf("the refusal wrote %d ledger rows, want 1", len(rows))
	}

	// A refused identity creates nothing and logs nobody in.
	if _, err := f.h.St.Users.FindByProvider(ssoProviderName, "subject-nobody"); err == nil {
		t.Error("the refusal created an account")
	}
}

func TestSSOIdentitiesRoutesAreBearerOnly(t *testing.T) {
	f := newSSOFixture(t, ssoClaims(), nil)
	router := routerFor(t, f.h)
	admin, _ := f.h.St.Users.GetByUsername("admin")
	token := accessToken(t, f.h.St, f.h.JWT, admin)

	// The token in the query is not a credential on REST routes (S2-08): the admin
	// surface is exactly where that rule matters, because these URLs are the ones
	// that get pasted into a chat.
	status, body, _ := do(t, router, request{method: http.MethodGet, path: "/api/identities", query: "token=" + token})
	if status != http.StatusUnauthorized {
		t.Fatalf("a queue read with ?token= answered %d %v, want 401", status, body)
	}
	if rows := auditRows(t, f.h.St, models.AuditIdentityRead); len(rows) != 0 {
		t.Errorf("an unauthenticated queue read wrote %d ledger rows, want 0", len(rows))
	}
}

func TestSSOSessionRoutesStayBehindTheBearerGate(t *testing.T) {
	f := newSSOFixture(t, ssoClaims(), nil)
	status, authorize := f.get(t, f.server.URL+"/api/auth/sso/"+ssoProviderName+"/authorize")
	if status != http.StatusFound {
		t.Fatalf("authorize answered %d", status)
	}
	if strings.Contains(authorize, "access_token=") || strings.Contains(authorize, "refresh_token=") {
		t.Error("the authorize redirect carries a token")
	}
}
