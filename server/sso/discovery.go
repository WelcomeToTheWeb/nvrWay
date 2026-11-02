package sso

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OIDC discovery (2.4-1).
//
// An operator configures an identity provider by naming it — the issuer — and
// the provider publishes the rest. This file is the difference between "add
// Keycloak in a day" and "paste 6 URLs and hope", and it is the reason 2.4-2 can
// verify an id_token at all: `jwks_uri` comes from here.
//
// The validation is deliberately loud. A provider row saved from a partial
// discovery document is a login that fails at the worst possible moment, with a
// 500 whose cause lives on someone else's server. Discovery is run at **save
// time**, and every failure below is a named error the console can show.

// Discovery failure kinds. The console maps each to its own state (2.4-4's
// "test-connection distinguishes discovery failure, redirect mismatch, and JWKS
// failure"), so these are part of the API contract, not internal detail.
const (
	DiscoveryOK             = "ok"
	DiscoveryUnreachable    = "discovery_unreachable"
	DiscoveryBadDocument    = "discovery_document"
	DiscoveryIssuerMismatch = "issuer_mismatch"
	DiscoveryNoCodeFlow     = "no_code_flow"
)

var (
	// ErrDiscoveryUnreachable: the document could not be fetched at all.
	ErrDiscoveryUnreachable = errors.New("discovery unreachable")
	// ErrDiscoveryDocument: fetched, but not a usable OIDC configuration.
	ErrDiscoveryDocument = errors.New("discovery document incomplete")
	// ErrIssuerMismatch: the document describes a different issuer than the one
	// configured. This is the mis-typed-issuer case, and the case where an
	// operator points nvrWay at a proxy that answers with a static document.
	ErrIssuerMismatch = errors.New("discovery issuer does not match the configured issuer")
	// ErrNoCodeFlow: the provider does not support the authorization-code flow,
	// which is the only flow nvrWay implements.
	ErrNoCodeFlow = errors.New("provider does not support the authorization code flow")
)

// DiscoveryKindOf names the discovery state an error carries, so the API answers
// the console's named state (`discovery_unreachable`, `issuer_mismatch`, …)
// instead of a 500 whose cause is a log line. An error that is not a
// DiscoveryError is the unreachable case: it is a fetch that produced no
// document, which is what the operator has to fix.
func DiscoveryKindOf(err error) string {
	var discoveryErr *DiscoveryError
	if errors.As(err, &discoveryErr) {
		return discoveryErr.Kind
	}
	if err == nil {
		return DiscoveryOK
	}
	return DiscoveryUnreachable
}

// DiscoveryDocument is the subset of `.well-known/openid-configuration` the
// product acts on. Unknown members are ignored (the document is the IdP's, and
// it will publish more than we use); `Raw` keeps the document verbatim so the
// console can show the operator exactly what was discovered before they save.
type DiscoveryDocument struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	UserInfoEndpoint      string   `json:"userinfo_endpoint"`
	JwksURI               string   `json:"jwks_uri"`
	ResponseTypes         []string `json:"response_types_supported"`
	ScopesSupported       []string `json:"scopes_supported"`
	PKCEMethods           []string `json:"code_challenge_methods_supported"`
	// IDTokenSigningAlgs is what the IdP says it signs id_tokens with. Our
	// verifier accepts RS256 only (2.4-2), so an IdP publishing only ES256 is a
	// provider that will log people in and then refuse every token — and the
	// operator has to be told that at setup, from the preview, not at 2 a.m.
	IDTokenSigningAlgs []string `json:"id_token_signing_alg_values_supported"`
	Raw                string   `json:"-"`
}

// DiscoveryError carries a failure kind beside the message, so the API answers
// a named state instead of a 500 and a log line.
type DiscoveryError struct {
	Kind string
	Err  error
}

func (e *DiscoveryError) Error() string { return e.Err.Error() }
func (e *DiscoveryError) Unwrap() error { return e.Err }

// Discover fetches and validates the OIDC configuration for an issuer.
//
// The issuer's document location is `<issuer>/.well-known/openid-configuration`
// per the spec — the one rule that makes "any conformant IdP" true. An explicit
// discovery URL wins when the operator gives one (some IdPs publish the document
// under a path that is not issuer + well-known).
func Discover(client *http.Client, issuer, discoveryURL string) (*DiscoveryDocument, error) {
	docURL := strings.TrimSpace(discoveryURL)
	if docURL == "" {
		docURL = strings.TrimRight(strings.TrimSpace(issuer), "/") + "/.well-known/openid-configuration"
	}

	resp, err := client.Get(docURL)
	if err != nil {
		return nil, &DiscoveryError{DiscoveryUnreachable,
			fmt.Errorf("%w: %s: %v", ErrDiscoveryUnreachable, docURL, err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &DiscoveryError{DiscoveryUnreachable,
			fmt.Errorf("%w: %s answered %d", ErrDiscoveryUnreachable, docURL, resp.StatusCode)}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &DiscoveryError{DiscoveryUnreachable,
			fmt.Errorf("%w: reading %s: %v", ErrDiscoveryUnreachable, docURL, err)}
	}

	var doc DiscoveryDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, &DiscoveryError{DiscoveryBadDocument,
			fmt.Errorf("%w: %s is not JSON: %v", ErrDiscoveryDocument, docURL, err)}
	}
	doc.Raw = string(body)

	// The three endpoints our flow calls, every time someone logs in. A
	// document without them is not an OIDC provider we can drive.
	for name, value := range map[string]string{
		"authorization_endpoint": doc.AuthorizationEndpoint,
		"token_endpoint":         doc.TokenEndpoint,
		"jwks_uri":               doc.JwksURI,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, &DiscoveryError{DiscoveryBadDocument,
				fmt.Errorf("%w: %s has no %s", ErrDiscoveryDocument, docURL, name)}
		}
	}
	// The flow we implement is authorization-code with a userinfo call. An IdP
	// that publishes no userinfo endpoint can still be used with an id_token
	// (2.4-2), so this is not refused here; an endpoint that is not an absolute
	// URL is, because our client cannot call it.
	for name, value := range map[string]string{
		"authorization_endpoint": doc.AuthorizationEndpoint,
		"token_endpoint":         doc.TokenEndpoint,
		"user_info_endpoint":     doc.UserInfoEndpoint,
		"jwks_uri":               doc.JwksURI,
	} {
		if value == "" {
			continue
		}
		u, err := url.Parse(value)
		if err != nil || !u.IsAbs() {
			return nil, &DiscoveryError{DiscoveryBadDocument,
				fmt.Errorf("%w: %s is not an absolute URL in the document from %s",
					ErrDiscoveryDocument, name, docURL)}
		}
	}

	// The document must describe the issuer the operator named. Without this
	// check a typo ("https://id.example.com/" vs "https://id.example.com")
	// saves, and the tokens it mints are rejected by 2.4-2's `iss` check with a
	// message that points at the IdP, not at the row.
	if strings.TrimSpace(doc.Issuer) == "" {
		return nil, &DiscoveryError{DiscoveryBadDocument,
			fmt.Errorf("%w: %s has no issuer", ErrDiscoveryDocument, docURL)}
	}
	if strings.TrimRight(doc.Issuer, "/") != strings.TrimRight(strings.TrimSpace(issuer), "/") {
		return nil, &DiscoveryError{DiscoveryIssuerMismatch,
			fmt.Errorf("%w: document says %q, configured %q", ErrIssuerMismatch, doc.Issuer, issuer)}
	}

	// `code` must be offered. IdPs that publish only implicit flows cannot be
	// driven by a server-side confidential client.
	if !containsFold(doc.ResponseTypes, "code") {
		return nil, &DiscoveryError{DiscoveryNoCodeFlow,
			fmt.Errorf("%w: response_types_supported is %v", ErrNoCodeFlow, doc.ResponseTypes)}
	}

	return &doc, nil
}

// ScopeSupports reports whether the IdP says it honors a scope. The `email`
// scope is the one that decides whether an asserted email is meaningful, and
// `offline_access` is the one that decides whether a session survives — so the
// console can warn an operator before their users notice.
func (d *DiscoveryDocument) ScopeSupports(scope string) bool {
	return containsFold(d.ScopesSupported, scope)
}

// SupportsSigningAlg reports whether the IdP publishes an id_token signing
// algorithm. An IdP that publishes no `id_token_signing_alg_values_supported` is
// treated as RS256-only, which is what the OIDC discovery spec requires of a
// conformant provider.
func (d *DiscoveryDocument) SupportsSigningAlg(alg string) bool {
	return containsFold(d.IDTokenSigningAlgs, alg)
}

// PKCESupported reports whether the IdP accepts a code-challenge method. PKCE
// closes the "authorization code intercepted in transit" hole on a public
// client; on a confidential one it is defence in depth (2.4-2).
func (d *DiscoveryDocument) PKCESupported() bool {
	return len(d.PKCEMethods) > 0
}

func containsFold(values []string, want string) bool {
	for _, v := range values {
		if strings.EqualFold(strings.TrimSpace(v), want) {
			return true
		}
	}
	return false
}

// DefaultHTTPClient is the client discovery and the SSO calls share: a timeout,
// because an unreachable IdP must produce a named failure in seconds, not a
// request that hangs a login.
func DefaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}
