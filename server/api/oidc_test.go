package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"nvr/server/models"
	"nvr/server/sso"
	"nvr/server/sso/testidp/provider"
)

// Step 2's last suite: the OIDC surface as the API answers it, in the shapes the
// plan's exit criteria name.
//
// The `sso/` suites test the contract — code exchange, signed-token verification,
// the identity gate. The `sso_routes_test.go` suite walks the routes. This file is
// about the two things neither of them can see on its own: what a provisioned
// identity **is allowed to do** once it exists (the `viewer` floor, and the write
// it is refused), and what each of the contract's refusals looks like **at the
// route** — in the console's fragment and in the ledger, not in a Go error.

// camerasShapedRoute mounts the permission gate the way Step 3 will mount it on a
// write route, so the gate is proven against a JIT-provisioned identity before a
// cameras route exists. The permission string, the gate and the ledger writer are
// the ones `api.Handlers` will use.
func camerasShapedRoute(h *Handlers) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	group := r.Group("/api/v1", AuthMiddleware(h.JWT))
	group.POST("/cameras", RequirePermission(models.PermCamerasWrite, h.St, h.St.Audit), func(c *gin.Context) {
		c.JSON(http.StatusCreated, gin.H{"status": "created"})
	})
	return r
}

func TestOIDCJITArrivesAtTheViewerFloorAndCannotWrite(t *testing.T) {
	fixture := newSSOFixture(t, ssoClaims(), nil)
	fixture.walk(t)

	user, err := fixture.h.St.Users.FindByProvider(ssoProviderName, "subject-ops")
	if err != nil || user == nil {
		t.Fatalf("the login provisioned no account: %v", err)
	}
	roles, err := fixture.h.St.Users.RolesFor(user.ID)
	if err != nil {
		t.Fatalf("reading roles: %v", err)
	}
	if len(roles) != 1 || roles[0] != models.RoleViewer {
		t.Fatalf("JIT roles = %v, want exactly [viewer]", roles)
	}

	// The floor is not a label the console respects and the API does not. The
	// provider's groups are `viewers`; nothing in the mapping moved this identity
	// up, so its first write attempt is refused.
	handoff, _ := fixture.walk(t)
	_, fragment := handoffParts(t, handoff)
	access := fragment["access_token"]

	status, body, _ := do(t, camerasShapedRoute(fixture.h), request{
		method: http.MethodPost, path: "/api/v1/cameras", bearer: access,
		body: map[string]any{"name": "Door camera", "slug": "door"},
	})
	if status != http.StatusForbidden {
		t.Fatalf("a viewer writing a camera: got %d %v, want 403", status, body)
	}
	if body["error"] != "insufficient role" {
		t.Errorf("the refusal answered %v, want \"insufficient role\"", body["error"])
	}

	// The refusal is a ledger row, so "who tried to add a camera at 3 a.m." is a
	// query, not a guess.
	refused := 0
	for _, row := range auditRows(t, fixture.h.St, models.AuditCameraCreate) {
		if row.Outcome == models.AuditOutcomeRefused {
			refused++
		}
	}
	if refused == 0 {
		t.Fatal("the permission refusal wrote no ledger row")
	}

	// The floor is a role row, not a claim: what the gate refused is the
	// permission, and what the account has is the read set the floor grants.
	rolesNow, err := fixture.h.St.Users.RolesFor(user.ID)
	if err != nil || len(rolesNow) != 1 || rolesNow[0] != models.RoleViewer {
		t.Errorf("the account's role rows are %v, want exactly [viewer]", rolesNow)
	}
}

// TestOIDCContractRefusalsArriveAtTheCallbackAsTheirOwnStates is the plan's
// criterion in the shape the console needs: the five refusals are five different
// screens and five different fixes on the IdP side, so the callback must answer
// each with its own name, and record it.
func TestOIDCContractRefusalsArriveAtTheCallbackAsTheirOwnStates(t *testing.T) {
	cases := []struct {
		name   string
		claims provider.Claims
		mutate func(*models.SSOProvider)
		want   string
	}{
		{
			// The fixture fills in a subject when the claims carry none, so the
			// assertion has to be sabotaged where it is published: the `sub` the
			// userinfo call and the signed token both publish is overridden to a
			// value that is not a string, which is the shape that arrives from a
			// real IdP whose claim mapping points at a claim that does not exist.
			name: "an assertion with no subject",
			claims: provider.Claims{
				Subject: "subject-nobody", Email: "nobody@nvrway.test", EmailVerified: true,
				Username: "nobody", Extra: map[string]any{"sub": nil},
			},
			want: sso.ReasonSubjectMissing,
		},
		{
			name: "an IdP whose signed token and userinfo disagree",
			claims: provider.Claims{
				Subject: "subject-real", IDTokenSubject: "subject-other", TokenSabotage: "sub_mismatch",
				Email: "ops@nvrway.test", EmailVerified: true, Username: "ops",
			},
			want: sso.ReasonClaimsDisagree,
		},
		{
			name: "an email outside the operator's allow-list",
			claims: provider.Claims{
				Subject: "subject-outsider", Email: "ops@other.example", EmailVerified: true, Username: "ops",
			},
			want: sso.ReasonDomainNotAllowed,
		},
		{
			name: "an unverified email the provider requires verified",
			claims: provider.Claims{
				Subject: "subject-unverified", Email: "ops@nvrway.test", EmailVerified: false, Username: "ops",
			},
			want: sso.ReasonEmailUnverified,
		},
		{
			name: "an assertion whose email belongs to an existing account",
			claims: provider.Claims{
				Subject: "subject-matching-email", Email: "matching@nvrway.test", EmailVerified: true,
				Username: "matching",
			},
			want: sso.ReasonIdentityPending,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newSSOFixture(t, tc.claims, tc.mutate)

			// The account the last case points at: an email the assertion can
			// claim, in an installation whose default link policy is `pending`.
			if tc.want == sso.ReasonIdentityPending {
				existing := &models.User{
					Username: "the-owner", Email: "matching@nvrway.test",
					Provider: models.ProviderLocal, Status: models.UserStatusActive,
				}
				if err := fixture.h.St.Users.Create(existing); err != nil {
					t.Fatalf("creating the account the assertion points at: %v", err)
				}
			}

			handoff, _ := fixture.walk(t)
			_, fragment := handoffParts(t, handoff)
			got := fragment["error"]
			if got != tc.want {
				t.Fatalf("the callback answered error=%q, want %q (full fragment: %v)", got, tc.want, fragment)
			}
			if strings.Contains(handoff, "access_token=") {
				t.Errorf("the refusal handed off session tokens: %q", handoff)
			}

			rows := auditRows(t, fixture.h.St, models.AuditSSOLoginRefused)
			if len(rows) == 0 {
				t.Fatal("the refusal wrote no ledger row")
			}
			if !strings.Contains(rows[0].Details, tc.want) {
				t.Errorf("the ledger row does not name the reason: %s", rows[0].Details)
			}
			if tc.want == sso.ReasonIdentityPending && !strings.Contains(rows[0].Details, "pending_identity_id") {
				t.Errorf("the pending refusal's row does not name the queue row it created: %s", rows[0].Details)
			}
			if tc.want == sso.ReasonIdentityPending && fragment["pending_identity"] == "" {
				t.Error("the console was not told which queue row to show the operator")
			}
		})
	}
}

func TestOIDCHandoffPairRotatesOnTheRefreshRoute(t *testing.T) {
	fixture := newSSOFixture(t, ssoClaims(), nil)
	handoff, _ := fixture.walk(t)
	_, fragment := handoffParts(t, handoff)
	first, refresh := fragment["access_token"], fragment["refresh_token"]

	router := routerFor(t, fixture.h)
	status, rotated, _ := do(t, router, request{
		method: http.MethodPost, path: "/api/auth/refresh",
		body: map[string]any{"refresh_token": refresh},
	})
	if status != http.StatusOK {
		t.Fatalf("refreshing an SSO-issued session: got %d %v, want 200", status, rotated)
	}
	second, _ := rotated["refresh_token"].(string)
	if second == "" || second == refresh {
		t.Fatalf("the OIDC session did not rotate: %v", rotated)
	}

	// The rotation's paper trail: the row it closed, and the row that names it.
	old, err := fixture.h.St.Sessions.GetByTokenHash(models.HashRefreshToken(refresh))
	if err != nil || old == nil {
		t.Fatalf("the rotated row is unreadable: %v", err)
	}
	if old.RevokedAt == nil {
		t.Error("an SSO session's rotated row is still live")
	}
	replacement, err := fixture.h.St.Sessions.GetByTokenHash(models.HashRefreshToken(second))
	if err != nil || replacement == nil {
		t.Fatalf("the replacement row is missing: %v", err)
	}
	if replacement.RotatedFrom != old.TokenHash {
		t.Errorf("rotated_from = %q, want the row it replaced", replacement.RotatedFrom)
	}

	// The reuse rule applies to an SSO session exactly as it does to a password
	// session: a copy of a spent token is a stolen token, and the account's live
	// sessions go.
	status, reused, _ := do(t, router, request{
		method: http.MethodPost, path: "/api/auth/refresh",
		body: map[string]any{"refresh_token": refresh},
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("replay of an SSO refresh token: got %d %v, want 401", status, reused)
	}
	if reused["error"] != "refresh token was already used" {
		t.Errorf("the replay answered %v, want \"refresh token was already used\"", reused["error"])
	}
	if dropped, _ := reused["live_sessions_dropped"].(float64); dropped < 1 {
		t.Errorf("live_sessions_dropped = %v, want at least 1", reused["live_sessions_dropped"])
	}

	// The handoff's access token is unaffected by the reuse, and expires on its
	// own: the design's answer to a stolen bearer token is the 30-minute TTL.
	status, _, _ = do(t, router, request{method: http.MethodGet, path: "/api/auth/profile", bearer: first})
	if status != http.StatusOK {
		t.Fatalf("the handoff's access token stopped working on reuse: got %d, want 200 until expiry", status)
	}
}

// TestOIDCDiscoveryFailuresAreRecordedOnTheRowTheConsoleReads: the four states a
// provider row can carry, written by the same call the console reads.
//
// A provider that failed discovery is a login button that fails at the IdP, and
// the operator's question ("why is there no login?") has to be answered by the
// row, not by a log line they have to be told how to grep.
func TestOIDCDiscoveryFailuresAreRecordedOnTheRowTheConsoleReads(t *testing.T) {
	cases := []struct {
		breakKind string
		wantState string
	}{
		{breakKind: "off", wantState: sso.DiscoveryUnreachable},
		{breakKind: "garbage", wantState: sso.DiscoveryBadDocument},
		{breakKind: "issuer", wantState: sso.DiscoveryIssuerMismatch},
		{breakKind: "no_code", wantState: sso.DiscoveryNoCodeFlow},
	}

	for _, tc := range cases {
		fixture := newSSOFixture(t, ssoClaims(), nil)
		fixture.idp.BreakDocument(tc.breakKind)

		_, err := sso.Discover(sso.DefaultHTTPClient(), fixture.idp.Issuer(), "")
		if err == nil {
			t.Fatalf("discovery against a %q document answered no error", tc.breakKind)
		}
		var discoveryErr *sso.DiscoveryError
		if !asDiscovery(err, &discoveryErr) {
			t.Fatalf("discovery answered %T (%v), want a *sso.DiscoveryError", err, err)
		}
		if discoveryErr.Kind != tc.wantState {
			t.Errorf("the failure kind is %q, want %q", discoveryErr.Kind, tc.wantState)
		}

		// The row carries the state, and the login page's list carries the row.
		if err := fixture.h.St.SSOProviders.MarkDiscovery(fixture.provider.ID, tc.wantState, "", fixture.h.now().UTC(), false); err != nil {
			t.Fatalf("recording the discovery state: %v", err)
		}
		status, body, _ := do(t, routerFor(t, fixture.h), request{method: http.MethodGet, path: "/api/auth/sso/providers"})
		if status != http.StatusOK {
			t.Fatalf("the provider list answered %d %v, want 200", status, body)
		}
		list, _ := body["providers"].([]any)
		if len(list) != 1 {
			t.Fatalf("the provider list is %v, want the one row", body["providers"])
		}
		row := list[0].(map[string]any)
		if row["discovery"] != tc.wantState {
			t.Errorf("the login page was told %v, want %q", row["discovery"], tc.wantState)
		}
	}

	// A conformant provider records `ok`, and the JWKS/endpoint facts come from the
	// document — the reason a provider row is saved from discovery, not typed.
	okFixture := newSSOFixture(t, ssoClaims(), nil)
	doc, err := sso.Discover(sso.DefaultHTTPClient(), okFixture.idp.Issuer(), "")
	if err != nil {
		t.Fatalf("discovery against the conformant fixture: %v", err)
	}
	if err := okFixture.h.St.SSOProviders.MarkDiscovery(okFixture.provider.ID, sso.DiscoveryOK, doc.Raw, okFixture.h.now().UTC(), doc.PKCESupported()); err != nil {
		t.Fatalf("recording the discovery: %v", err)
	}
	stored, err := okFixture.h.St.SSOProviders.GetByName(ssoProviderName)
	if err != nil {
		t.Fatalf("reading the row: %v", err)
	}
	if stored.DiscoveryState != sso.DiscoveryOK || !stored.PKCESupported {
		t.Errorf("the row carries state %q / pkce %v, want ok and true", stored.DiscoveryState, stored.PKCESupported)
	}
}

// TestOIDCIdentityRoutesAreTheAdminsAndNobodyElses closes the pair the plan names:
// the queue is an administrator's surface, and a JIT-provisioned identity — the
// account the queue creates — cannot administer itself.
func TestOIDCIdentityRoutesAreTheAdminsAndNobodyElse(t *testing.T) {
	fixture := newSSOFixture(t, ssoClaims(), nil)
	fixture.walk(t)
	handoff, _ := fixture.walk(t)
	_, fragment := handoffParts(t, handoff)
	access := fragment["access_token"]

	router := routerFor(t, fixture.h)
	// The identity the login just provisioned is a `viewer`: reading the queue is
	// an admin act, and a JIT account has no admin role to read it with.
	status, body, _ := do(t, router, request{method: http.MethodGet, path: "/api/identities", bearer: access})
	if status != http.StatusForbidden {
		t.Fatalf("a JIT viewer reading the queue: got %d %v, want 403", status, body)
	}
	if body["error"] != "insufficient role" {
		t.Errorf("the answer is %v, want \"insufficient role\"", body["error"])
	}

	// The identity row the JIT login created is not in the queue at all: only the
	// assertions the contract refused are.
	if rows, err := fixture.h.St.PendingIdentities.ListPending(); err != nil || len(rows) != 0 {
		t.Errorf("the queue holds %d rows after a clean JIT login, want 0 (err %v)", len(rows), err)
	}
}

// asDiscovery asserts a *named* discovery failure — a `*sso.DiscoveryError` with a
// `Kind` the row can record — rather than an error whose message happens to mention
// a state. A console cannot draw a state from a string it has to substring-match.
func asDiscovery(err error, target **sso.DiscoveryError) bool {
	return errors.As(err, target)
}
