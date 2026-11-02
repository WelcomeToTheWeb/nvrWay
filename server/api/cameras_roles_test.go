package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nvr/server/models"
	"nvr/server/sso"
)

// S3-06 — the role floor, asserted as a matrix.
//
// The floor is the plan's (Step 3): `viewer` reads cameras, events and clips;
// `operator` adds camera CRUD and the probe; `admin` adds the accounts, the media
// tier, the webhooks and the OIDC providers.
//
// It is asserted as a matrix rather than one test per route because the failure
// this guards is a single missing gate on a single route, and a per-route test
// that a later card forgets to add is exactly how that happens. The table is the
// contract: adding a route means adding a row, and the row says which role gets
// which status.
//
// Every refusal is asserted to be a ledger row under the route's declared verb
// (`docs/kanban.md`: a 403 the operator cannot reconstruct is a 403 that becomes
// someone reading logs at 2 a.m.).

func TestCamerasRoleFloorMatrix(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)
	fx.Media.setStreams(`{"front-door": {"producers": ["rtsp://cam:554/live"]}}`)

	tok := map[string]string{
		models.RoleViewer:   fx.ViewerTok,
		models.RoleOperator: fx.OperatorTok,
		models.RoleAdmin:    fx.AdminTok,
	}

	type routeCase struct {
		name   string
		method string
		path   string
		body   func(role string) any
		want   map[string]int
	}

	cases := []routeCase{
		{
			name: "camera list", method: http.MethodGet, path: "/api/v1/cameras",
			want: map[string]int{models.RoleViewer: http.StatusOK, models.RoleOperator: http.StatusOK, models.RoleAdmin: http.StatusOK},
		},
		{
			name: "camera detail", method: http.MethodGet, path: "/api/v1/cameras/" + id,
			want: map[string]int{models.RoleViewer: http.StatusOK, models.RoleOperator: http.StatusOK, models.RoleAdmin: http.StatusOK},
		},
		{
			name: "camera snapshot", method: http.MethodGet, path: "/api/v1/cameras/" + id + "/snapshot",
			want: map[string]int{models.RoleViewer: http.StatusOK, models.RoleOperator: http.StatusOK, models.RoleAdmin: http.StatusOK},
		},
		{
			// The operator floor is the whole point of it: a viewer who can add a
			// camera is a viewer who can point the recorder at anything.
			name: "camera create", method: http.MethodPost, path: "/api/v1/cameras",
			body: func(role string) any { return cameraBody("cam-"+role, "rtsp://cam:554/live") },
			want: map[string]int{models.RoleViewer: http.StatusForbidden, models.RoleOperator: http.StatusCreated, models.RoleAdmin: http.StatusCreated},
		},
		{
			name: "camera update", method: http.MethodPut, path: "/api/v1/cameras/" + id,
			body: func(role string) any { return map[string]any{"name": "Renamed by " + role} },
			want: map[string]int{models.RoleViewer: http.StatusForbidden, models.RoleOperator: http.StatusOK, models.RoleAdmin: http.StatusOK},
		},
		{
			name: "camera probe", method: http.MethodPost, path: "/api/v1/cameras/" + id + "/probe",
			want: map[string]int{models.RoleViewer: http.StatusForbidden, models.RoleOperator: http.StatusOK, models.RoleAdmin: http.StatusOK},
		},
		{
			name: "camera zones", method: http.MethodPut, path: "/api/v1/cameras/" + id + "/zones",
			body: func(role string) any { return []map[string]any{zoneBody("z-" + role)} },
			want: map[string]int{models.RoleViewer: http.StatusForbidden, models.RoleOperator: http.StatusOK, models.RoleAdmin: http.StatusOK},
		},
		{
			// Deleting a camera destroys its footage ledger: not an operator act.
			name: "camera delete", method: http.MethodDelete, path: "/api/v1/cameras/" + id,
			want: map[string]int{models.RoleViewer: http.StatusForbidden, models.RoleOperator: http.StatusForbidden, models.RoleAdmin: http.StatusOK},
		},
	}

	for _, tc := range cases {
		for _, role := range []string{models.RoleViewer, models.RoleOperator, models.RoleAdmin} {
			var body any
			if tc.body != nil {
				body = tc.body(role)
			}
			status, out, raw := matrixDo(t, fx.Router, tc.method, tc.path, body, tok[role])
			want := tc.want[role]
			if status != want {
				t.Fatalf("%s as %s: got %d %s, want %d", tc.name, role, status, raw, want)
			}
			if want == http.StatusForbidden {
				if out["error"] != "insufficient role" {
					t.Fatalf("%s as %s: the refusal is the named one, got %v", tc.name, role, out)
				}
			}
		}
	}

	// No token at all: the surface is protected, and "protected" means 401 before
	// any role question is asked.
	for _, tc := range cases {
		var body any
		if tc.body != nil {
			body = tc.body("anonymous")
		}
		status, _, _ := matrixDo(t, fx.Router, tc.method, tc.path, body, "")
		if status != http.StatusUnauthorized {
			t.Fatalf("%s with no token: got %d, want 401", tc.name, status)
		}
	}
}

// TestCamerasRoleMatrixDenialsAreAudited: every 403 the matrix produced is a
// ledger row under the verb the route declared, with the actor named.
func TestCamerasRoleMatrixDenialsAreAudited(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)
	fx.Media.setStreams(`{"front-door": {"producers": ["rtsp://cam:554/live"]}}`)

	denials := []struct {
		method string
		path   string
		body   any
		bearer string
		actor  string
		action string
	}{
		{http.MethodPost, "/api/v1/cameras", cameraBody("viewer-tries", "rtsp://cam:554/live"), fx.ViewerTok, "cam-viewer", models.AuditCameraCreate},
		{http.MethodPut, "/api/v1/cameras/" + id, map[string]any{"name": "viewer renames"}, fx.ViewerTok, "cam-viewer", models.AuditCameraUpdate},
		{http.MethodPost, "/api/v1/cameras/" + id + "/probe", nil, fx.ViewerTok, "cam-viewer", models.AuditCameraProbe},
		{http.MethodPut, "/api/v1/cameras/" + id + "/zones", []map[string]any{zoneBody("z1")}, fx.ViewerTok, "cam-viewer", models.AuditCameraZones},
		{http.MethodDelete, "/api/v1/cameras/" + id, nil, fx.ViewerTok, "cam-viewer", models.AuditCameraDelete},
		{http.MethodDelete, "/api/v1/cameras/" + id, nil, fx.OperatorTok, "cam-operator", models.AuditCameraDelete},
	}

	for i, d := range denials {
		status, _, _ := do(t, fx.Router, request{method: d.method, path: d.path, body: d.body, bearer: d.bearer})
		if status != http.StatusForbidden {
			t.Fatalf("denial %d: got %d, want 403", i, status)
		}
		rows := auditRows(t, fx.Store, d.action)
		found := false
		for _, row := range rows {
			if row.Outcome != models.AuditOutcomeRefused || row.ActorUsername != d.actor {
				continue
			}
			// A refused create names the route (there is no camera id yet); every
			// other refusal is about one camera and names it.
			if d.method == http.MethodPost && d.path == "/api/v1/cameras" {
				found = row.TargetType == "route"
			} else {
				found = row.TargetID == id
			}
			if found {
				break
			}
		}
		if !found {
			t.Fatalf("denial %d (%s %s by %s) is not a refused %s row naming the camera: %+v",
				i, d.method, d.path, d.actor, d.action, rows)
		}
	}
}

// TestCamerasOIDCSettingsIsAdminOnly: the plan's floor puts the OIDC provider
// surface (`GET/POST/PUT/DELETE /api/settings/oidc`) on `admin`, because who
// people can log in as is decided there.
func TestCamerasOIDCSettingsIsAdminOnly(t *testing.T) {
	fx := newCameraFixture(t)

	issuer, stop := discoveryIssuer(t, "corp")
	defer stop()
	created := createdOIDCProvider(t, fx, "corp", issuer, "nvrway-client")

	cases := []struct {
		name   string
		method string
		path   string
		body   func(role string) any
		want   map[string]int
		action string
	}{
		{
			name: "provider list", method: http.MethodGet, path: "/api/settings/oidc",
			want:   map[string]int{models.RoleViewer: http.StatusForbidden, models.RoleOperator: http.StatusForbidden, models.RoleAdmin: http.StatusOK},
			action: models.AuditOIDCRead,
		},
		{
			name: "provider create", method: http.MethodPost, path: "/api/settings/oidc",
			body: func(role string) any {
				return map[string]any{
					"name": "idp-" + role, "issuer": issuer, "client_id": "nvrway-client",
					"client_secret": "a-client-secret-long-enough",
				}
			},
			want:   map[string]int{models.RoleViewer: http.StatusForbidden, models.RoleOperator: http.StatusForbidden, models.RoleAdmin: http.StatusCreated},
			action: models.AuditOIDCCreate,
		},
		{
			name: "provider update", method: http.MethodPut, path: "/api/settings/oidc/corp",
			body:   func(role string) any { return map[string]any{"scope": "openid email profile"} },
			want:   map[string]int{models.RoleViewer: http.StatusForbidden, models.RoleOperator: http.StatusForbidden, models.RoleAdmin: http.StatusOK},
			action: models.AuditOIDCUpdate,
		},
		{
			name: "provider delete", method: http.MethodDelete, path: "/api/settings/oidc/corp",
			want:   map[string]int{models.RoleViewer: http.StatusForbidden, models.RoleOperator: http.StatusForbidden, models.RoleAdmin: http.StatusOK},
			action: models.AuditOIDCDelete,
		},
	}

	for _, tc := range cases {
		for _, role := range []string{models.RoleViewer, models.RoleOperator, models.RoleAdmin} {
			var body any
			if tc.body != nil {
				body = tc.body(role)
			}
			status, out, raw := do(t, fx.Router, request{method: tc.method, path: tc.path, body: body, bearer: roleToken(t, fx, role)})
			want := tc.want[role]
			if status != want {
				t.Fatalf("%s as %s: got %d %s, want %d", tc.name, role, status, raw, want)
			}
			if want != http.StatusForbidden {
				continue
			}
			if out["error"] != "insufficient role" {
				t.Fatalf("%s as %s: the refusal is the named one, got %v", tc.name, role, out)
			}
			// The refusal is a ledger row under the verb this route group declared
			// in Step 2's denial registry.
			rows := auditRows(t, fx.Store, tc.action)
			found := false
			for _, row := range rows {
				if row.Outcome == models.AuditOutcomeRefused && row.ActorUsername == "cam-"+role {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("%s as %s: the refusal is not a refused %s row: %+v", tc.name, role, tc.action, rows)
			}
		}
	}

	// The admin's create landed (`idp-admin`), and the provider the matrix deleted
	// is gone: the refusals wrote nothing and the allowed write did.
	if _, err := fx.Store.SSOProviders.GetByName("idp-admin"); err != nil {
		t.Fatalf("the admin's create is the write that landed: %v", err)
	}
	if _, err := fx.Store.SSOProviders.GetByName(created); err == nil {
		t.Fatal("the admin's delete deleted")
	}
	for _, role := range []string{models.RoleViewer, models.RoleOperator} {
		if _, err := fx.Store.SSOProviders.GetByName("idp-" + role); err == nil {
			t.Fatalf("the %s refusal created a provider", role)
		}
	}
}

// TestCamerasUsersSurfaceIsAdminOnly: the other half of the plan's admin floor.
func TestCamerasUsersSurfaceIsAdminOnly(t *testing.T) {
	fx := newCameraFixture(t)

	for _, role := range []string{models.RoleViewer, models.RoleOperator} {
		status, _, _ := do(t, fx.Router, request{method: http.MethodGet, path: "/api/users", bearer: roleToken(t, fx, role)})
		if status != http.StatusForbidden {
			t.Fatalf("GET /api/users as %s: got %d, want 403", role, status)
		}
		rows := auditRows(t, fx.Store, models.AuditUserRead)
		refused := false
		for _, row := range rows {
			if row.Outcome == models.AuditOutcomeRefused && row.ActorUsername == "cam-"+role {
				refused = true
			}
		}
		if !refused {
			t.Fatalf("a refused account read is a ledger row (%s): %+v", role, rows)
		}
	}
	status, _, _ := do(t, fx.Router, request{method: http.MethodGet, path: "/api/users", bearer: fx.AdminTok})
	if status != http.StatusOK {
		t.Fatalf("GET /api/users as admin: got %d", status)
	}
}

// TestCamerasOIDCDiscoveryRunsAtSave: the provider surface asks the issuer before
// it stores the row, because a provider saved from a half-discovered issuer is a
// login that fails with a 500 whose cause lives on someone else's server.
func TestCamerasOIDCDiscoveryRunsAtSave(t *testing.T) {
	fx := newCameraFixture(t)

	// An issuer that answers nothing: the state is named, and no row lands.
	body := map[string]any{
		"name": "unreachable-idp", "issuer": "http://127.0.0.1:1",
		"client_id": "nvrway-client", "client_secret": "a-client-secret-long-enough",
	}
	status, out, _ := do(t, fx.Router, request{method: http.MethodPost, path: "/api/settings/oidc", body: body, bearer: fx.AdminTok})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("a provider whose issuer does not answer: got %d, want 422", status)
	}
	if out["discovery"] != sso.DiscoveryUnreachable {
		t.Fatalf("the console draws the named discovery state, got %v", out)
	}
	if _, err := fx.Store.SSOProviders.GetByName("unreachable-idp"); err == nil {
		t.Fatal("the refused provider was not saved")
	}

	// An issuer that answers: the endpoints come from discovery, not from typing.
	issuer, stop := discoveryIssuer(t, "okidp")
	defer stop()
	created := createdOIDCProvider(t, fx, "okidp", issuer, "nvrway-client")

	status, view, raw := do(t, fx.Router, request{method: http.MethodGet, path: "/api/settings/oidc", bearer: fx.AdminTok})
	if status != http.StatusOK {
		t.Fatalf("provider list: %d %s", status, raw)
	}
	providers, _ := view["providers"].([]any)
	for _, p := range providers {
		row := p.(map[string]any)
		if row["name"] != created {
			continue
		}
		if row["discovery_state"] != sso.DiscoveryOK {
			t.Fatalf("discovery is recorded on the row: %v", row)
		}
		if row["auth_url"] == "" || row["token_url"] == "" || row["jwks_uri"] == "" {
			t.Fatalf("the endpoints come from discovery: %v", row)
		}
		// The redirect URI has one author: the server computes it, the operator
		// copies it into the IdP.
		if row["redirect_uri"] != SSORedirectURI(fx.Handlers.Cfg.WebURL, created) {
			t.Fatalf("the row answers the redirect URI to whitelist: %v", row)
		}
		if row["secret_set"] != true {
			t.Fatalf("the secret is reported as set, never echoed: %v", row)
		}
		if _, has := row["client_secret"]; has {
			t.Fatal("no read echoes a client secret")
		}
		return
	}
	t.Fatalf("the created provider is in the list: %v", view)
}

// discoveryIssuer is a stand-in IdP: enough of `.well-known/openid-configuration`
// for discovery to accept it — the code flow, S256 PKCE, RS256, and the three
// endpoints the login calls. The issuer is its own base URL, which is the rule
// discovery checks (`issuer` must match the document's own origin).
func discoveryIssuer(t *testing.T, name string) (string, func()) {
	t.Helper()

	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		doc := fmt.Sprintf(`{
			"issuer": %q,
			"authorization_endpoint": %q,
			"token_endpoint": %q,
			"userinfo_endpoint": %q,
			"jwks_uri": %q,
			"response_types_supported": ["code"],
			"code_challenge_methods_supported": ["S256"],
			"scopes_supported": ["openid","email","profile"],
			"id_token_signing_alg_values_supported": ["RS256"]
		}`, base, base+"/authorize", base+"/token", base+"/userinfo", base+"/certs")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(doc))
	})

	server := httptest.NewServer(mux)
	base = server.URL
	t.Cleanup(server.Close)
	return server.URL, server.Close
}

// matrixDo is `do` for the matrix: the snapshot answers JPEG bytes, and a
// matrix walk that dies on an undecodable body is a matrix that cannot cover the
// snapshot.
func matrixDo(t *testing.T, handler http.Handler, method, path string, body any, bearer string) (int, map[string]any, string) {
	t.Helper()
	rec := doRaw(t, handler, request{method: method, path: path, body: body, bearer: bearer})
	raw := rec.Body.String()
	out := map[string]any{}
	if strings.HasPrefix(strings.TrimSpace(raw), "{") {
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("%s %s: body is not an object (%d): %q", method, path, rec.Code, raw)
		}
	}
	return rec.Code, out, raw
}

func roleToken(t *testing.T, fx *cameraFixture, role string) string {
	t.Helper()
	switch role {
	case models.RoleAdmin:
		return fx.AdminTok
	case models.RoleOperator:
		return fx.OperatorTok
	default:
		return fx.ViewerTok
	}
}

func createdOIDCProvider(t *testing.T, fx *cameraFixture, name, issuer, clientID string) string {
	t.Helper()
	status, out, raw := do(t, fx.Router, request{
		method: http.MethodPost, path: "/api/settings/oidc", bearer: fx.AdminTok,
		body: map[string]any{
			"name": name, "issuer": issuer, "client_id": clientID,
			"client_secret": "a-client-secret-long-enough",
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("creating the provider: got %d %s", status, raw)
	}
	if out["secret_set"] != true {
		t.Fatalf("the created row reports its secret as set: %v", out)
	}
	return name
}
