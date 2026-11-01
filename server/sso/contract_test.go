package sso

import (
	"errors"
	"strings"
	"testing"
)

func TestReasonStringsAreExact(t *testing.T) {
	// The five refusals the card names, plus the contract's other two. These
	// strings are the console's states and the audit ledger's reason column: a
	// rename is a console that stops rendering a state and a ledger row nobody
	// can filter on.
	exact := map[string]string{
		"ReasonSubjectMissing":   ReasonSubjectMissing,
		"ReasonEmailUnverified":  ReasonEmailUnverified,
		"ReasonDomainNotAllowed": ReasonDomainNotAllowed,
		"ReasonClaimsDisagree":   ReasonClaimsDisagree,
		"ReasonProviderDisabled": ReasonProviderDisabled,
		"ReasonNoEmailClaim":     ReasonNoEmailClaim,
		"ReasonIdentityPending":  ReasonIdentityPending,
		"ReasonAccountPending":   ReasonAccountPending,
	}
	want := map[string]string{
		"ReasonSubjectMissing":   "subject_missing",
		"ReasonEmailUnverified":  "email_unverified",
		"ReasonDomainNotAllowed": "domain_not_allowed",
		"ReasonClaimsDisagree":   "id_token_userinfo_disagreement",
		"ReasonProviderDisabled": "provider_disabled",
		"ReasonNoEmailClaim":     "no_email_claim",
		"ReasonIdentityPending":  "identity_pending",
		"ReasonAccountPending":   "account_pending",
	}
	for name, got := range want {
		if exact[name] != got {
			t.Fatalf("%s is %q, want %q", name, exact[name], got)
		}
	}

	// No two refusals share a string: the console cannot render one state for
	// two different fixes on the IdP side.
	seen := map[string]string{}
	for name, value := range exact {
		if other, taken := seen[value]; taken {
			t.Fatalf("%s and %s share the reason string %q", name, other, value)
		}
		seen[value] = name
	}
	for name, value := range map[string]string{
		"ReasonCodeExchange":        ReasonCodeExchange,
		"ReasonUserInfoFailed":      ReasonUserInfoFailed,
		"ReasonMissingIDToken":      ReasonMissingIDToken,
		"ReasonIDTokenVerification": ReasonIDTokenVerification,
		"ReasonSubjectMismatch":     ReasonSubjectMismatch,
		"ReasonNonceMismatch":       ReasonNonceMismatch,
		"ReasonStateMismatch":       ReasonStateMismatch,
		"ReasonStateExpired":        ReasonStateExpired,
	} {
		if other, taken := seen[value]; taken {
			t.Fatalf("%s and %s share the reason string %q", name, other, value)
		}
		seen[value] = name
	}
}

func TestIdentityErrorCarriesTheReasonAndTheUnderlyingFailure(t *testing.T) {
	cause := errors.New("the idp asserted no subject")
	err := &IdentityError{
		Reason:       ReasonSubjectMissing,
		Subject:      "",
		Email:        "ida@corp.example",
		TargetUserID: "user-7",
		Err:          cause,
	}
	if !strings.HasPrefix(err.Error(), ReasonSubjectMissing+": ") {
		t.Fatalf("the message does not name the reason: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "the idp asserted no subject") {
		t.Fatalf("the message lost the cause: %q", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Fatal("the cause is not reachable through Unwrap")
	}
	if err.TargetUserID != "user-7" || err.Email != "ida@corp.example" {
		t.Fatal("the audit fields (who tried to arrive as whom) are not carried")
	}
}

func TestDomainAllowed(t *testing.T) {
	cases := []struct {
		name    string
		list    string
		email   string
		allowed bool
	}{
		{"an empty list is no restriction", "", "anyone@anywhere.example", true},
		{"a listed domain", "corp.example", "ida@corp.example", true},
		{"case-insensitive on both sides", " CORP.Example ", "Ida@Corp.EXAMPLE", true},
		{"a list of several", "corp.example, other.example", "ida@other.example", true},
		{"an unlisted domain", "corp.example", "ida@attacker.example", false},
		{"a suffix that is not the domain", "corp.example", "ida@corp.example.attacker.net", false},
		{"no at sign", "corp.example", "ida", false},
		{"nothing after the at sign", "corp.example", "ida@", false},
		{"an empty entry in the list does not allow everything", "corp.example,", "ida@nowhere.example", false},
	}
	for _, tc := range cases {
		if got := domainAllowed(tc.list, tc.email); got != tc.allowed {
			t.Fatalf("%s: domainAllowed(%q, %q) = %v, want %v", tc.name, tc.list, tc.email, got, tc.allowed)
		}
	}
}

func TestParseRoleMapping(t *testing.T) {
	if mapping := parseRoleMapping(""); mapping != nil {
		t.Fatalf("an empty mapping is no mapping, got %v", mapping)
	}
	if mapping := parseRoleMapping("   "); mapping != nil {
		t.Fatalf("whitespace is no mapping, got %v", mapping)
	}
	// An unparseable row is no mapping, not a grant: the save-time validation is
	// what refuses an operator's typo, and the runtime must not turn a stale row
	// into a privilege.
	for _, raw := range []string{"not json", `{"admins"}`, `["admins"]`, `{`, `13`} {
		if mapping := parseRoleMapping(raw); mapping != nil {
			t.Fatalf("unparseable mapping %q produced %v", raw, mapping)
		}
	}
	if mapping := parseRoleMapping(`{}`); mapping != nil {
		t.Fatalf("an empty object is no mapping, got %v", mapping)
	}
	// An entry with an empty claim or role is dropped, not carried as a wildcard.
	if mapping := parseRoleMapping(`{"": "admin", "admins": "", " ok ":" operator "}`); len(mapping) != 1 || mapping["ok"] != "operator" {
		t.Fatalf("mapping entries are not trimmed and emptied-out: %v", mapping)
	}
	if mapping := parseRoleMapping(`{"admins": "admin", "techs": "operator"}`); len(mapping) != 2 {
		t.Fatalf("mapping: %v", mapping)
	}
}

func TestMappingOwnsRolesIsTheSetTheMappingCanMove(t *testing.T) {
	mapping := parseRoleMapping(`{"admins": "admin", "techs": "operator", "guests": "operator"}`)
	owned := mappingOwnsRoles(mapping)
	if len(owned) != 2 {
		t.Fatalf("owned roles: %v", owned)
	}
	for _, role := range []string{"admin", "operator"} {
		if !owned[role] {
			t.Fatalf("the mapping can grant %s, so it must own it: %v", role, owned)
		}
	}
	if owned["viewer"] {
		t.Fatal("the mapping does not grant viewer, so it must not own the floor")
	}
	if owned = mappingOwnsRoles(nil); len(owned) != 0 {
		t.Fatalf("no mapping owns no roles, got %v", owned)
	}
}
