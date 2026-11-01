package sso

import (
	"encoding/json"
	"strings"
)

// The identity-linking contract (2.4-3).
//
// Generic OIDC widens the identity surface, and the JIT path had a behaviour a
// permissive IdP turns into account takeover: an asserted email that matched an
// existing local account **was** that account, including a password-authenticated
// admin. The roadmap settled the answer, and this file is it:
//
//   - an IdP may only assert identities in the domains the operator lists;
//   - `email_verified` is honored when the operator requires it, not parsed and
//     ignored;
//   - a new provider/email pair that points at an existing account becomes a
//     **pending identity** — no roles, no login, an admin's decision — and linking
//     an existing account is an explicit, per-provider choice (`link_policy=link`),
//     not a side effect of an email matching;
//   - role mapping is opt-in, and when configured it owns role assignment.
//
// One contract, two protocols: the SAML path (2.4-7) resolves identity through this
// same gate, not a second copy of it.

// Contract refusal reasons. Named, because a person locked out of their own account
// and an operator hunting a misconfigured IdP need the same string.
const (
	ReasonDomainNotAllowed = "domain_not_allowed"
	ReasonNoEmailClaim     = "no_email_claim"
	ReasonEmailUnverified  = "email_unverified"
	ReasonIdentityPending  = "identity_pending"
	ReasonAccountPending   = "account_pending"
)

// domainAllowed reports whether an asserted email is in the provider's allow-list.
// An empty list is no restriction (the pre-2.4 provider keeps working); a list is a
// wall. Comparison is on the email's domain part, case-insensitively, because the
// IdP owns the local part.
func domainAllowed(allowedDomains, email string) bool {
	list := strings.TrimSpace(allowedDomains)
	if list == "" {
		return true
	}
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return false
	}
	domain := strings.ToLower(strings.TrimSpace(email[at+1:]))
	for _, allowed := range strings.Split(list, ",") {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if allowed != "" && allowed == domain {
			return true
		}
	}
	return false
}

// parseRoleMapping reads the provider's role mapping: a JSON object of claim value →
// role name. An empty mapping is not "everyone is admin": it is no mapping, and the
// `viewer` floor stands. Unparseable input is no mapping too — the door validation
// (`validRoleMapping`) is what refuses an operator's typo at save time, and the
// runtime must not turn a stale row into a privilege grant.
func parseRoleMapping(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var mapping map[string]string
	if err := json.Unmarshal([]byte(raw), &mapping); err != nil {
		return nil
	}
	out := make(map[string]string, len(mapping))
	for claim, role := range mapping {
		claim = strings.TrimSpace(claim)
		role = strings.TrimSpace(role)
		if claim == "" || role == "" {
			continue
		}
		out[claim] = role
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mappingOwnsRoles lists the role names a mapping can grant. It is the set the
// mapping is allowed to **remove** as well as grant: a mapping that says "these
// groups are admin" means the technician who loses the group stops being admin,
// which is the whole reason an operator configures one.
func mappingOwnsRoles(mapping map[string]string) map[string]bool {
	owned := make(map[string]bool, len(mapping))
	for _, role := range mapping {
		owned[role] = true
	}
	return owned
}

// The refusal vocabulary. These strings are the console's states and the audit
// ledger's `reason` column, so they are part of the API contract: a person locked
// out of their own account and an operator hunting a misconfigured IdP need the
// same string, and "SSO login failed" is a message nobody can act on. Five
// refusals are five different fixes on the IdP side.
const (
	// ReasonSubjectMissing: the provider asserted an identity with no `sub`. The
	// subject is the only stable handle an IdP offers, so an assertion without
	// one cannot be provisioned, linked, or audited — and it is exactly what an
	// IdP that was misconfigured an hour ago sends.
	ReasonSubjectMissing = "subject_missing"
	// ReasonClaimsDisagree: the signed id_token and the userinfo call assert
	// different identities. nvrWay refuses the login rather than pick one,
	// because the disagreement is the signature of a provider that is
	// concatenating a token from one person with a profile from another.
	ReasonClaimsDisagree = "id_token_userinfo_disagreement"
	// ReasonProviderDisabled: the provider row is switched off. Refused at the
	// callback too, not only at the redirect: a login in flight when an operator
	// flips the switch must not complete.
	ReasonProviderDisabled = "provider_disabled"

	// The flow failures, named beside the contract's: the console renders each
	// as its own state, because "code exchange failed" and "nonce mismatch"
	// point at different machines.
	ReasonCodeExchange        = "code_exchange_failed"
	ReasonUserInfoFailed      = "userinfo_failed"
	ReasonMissingIDToken      = "missing_id_token"
	ReasonIDTokenVerification = "id_token_verification_failed"
	ReasonSubjectMismatch     = "subject_mismatch"
	ReasonNonceMismatch       = "nonce_mismatch"
	ReasonStateMismatch       = "state_mismatch"
	ReasonStateExpired        = "state_expired"
)

// IdentityError carries the refusal reason beside the message, so the callback
// answers a named state and writes that name into the ledger. Subject/Email are
// filled when the refusal happened *after* an identity was asserted — the audit
// question is not "was a login refused" but "who tried to arrive as whom".
type IdentityError struct {
	Reason  string
	Subject string
	Email   string
	// TargetUserID is the account an assertion pointed at — set for
	// identity_pending, where the whole point of the row is that someone's email
	// was used to try to arrive as them.
	TargetUserID string
	// PendingUserID is the identity record the contract created, when a refusal
	// created one. The admin who has to decide on it needs its id.
	PendingUserID string
	Err           error
}

func (e *IdentityError) Error() string { return e.Reason + ": " + e.Err.Error() }
func (e *IdentityError) Unwrap() error { return e.Err }
