// Command testidp runs nvrWay's in-repo OpenID Provider on a fixed address (2.4-1).
//
// It is the same provider the Go tests use (`nvr/server/sso/testidp/provider`),
// with the consent screen switched on: an operator pointing nvrWay at it, and a
// script walking authorize → consent → callback → token → userinfo, are the same
// acceptance test, one with a browser and one with curl.
//
// The flags are the four the card names — -listen, -client, -secret, -issuer — and
// the user the IdP asserts. Everything else is a switch a test needs: -break to
// sabotage the discovery document by hand, -require-pkce to prove a client sends a
// verifier, -no-consent to skip the screen.
//
//	docker run --rm -p 9500:9500 -v $PWD:/src -v nvr_gomodcache:/go/pkg/mod \
//	  -w /src golang:1.26 go run ./sso/testidp -listen :9500 \
//	  -client nvrdev -secret devsecret -issuer http://localhost:9500
package main

import (
	"flag"
	"fmt"
	"log"
	"strings"

	"nvr/server/sso/testidp/provider"
)

func main() {
	listen := flag.String("listen", ":9500", "address to serve on")
	clientID := flag.String("client", "nvrdev", "client_id the provider accepts")
	clientSecret := flag.String("secret", "devsecret", "client_secret the token endpoint requires")
	issuer := flag.String("issuer", "http://localhost:9500", "issuer the provider publishes (iss, and the endpoint base)")
	subject := flag.String("subject", "nvr-user-1", "sub the provider asserts")
	email := flag.String("email", "ops@nvrway.test", "email the provider asserts")
	name := flag.String("name", "Ada Ops", "name the provider asserts")
	username := flag.String("username", "ada.ops", "preferred_username the provider asserts")
	groups := flag.String("groups", "", "comma-separated group claim values (the role mapping reads these)")
	verified := flag.Bool("verified", true, "assert email_verified")
	requirePKCE := flag.Bool("require-pkce", false, "refuse a token request with no code_verifier")
	noConsent := flag.Bool("no-consent", false, "redirect straight back with a code, skipping the consent screen")
	sabotage := flag.String("break", "", "sabotage the discovery document: no_jwks | issuer | no_code | garbage | off | es256_only")
	flag.Parse()

	idp, addr, err := provider.Listen(*listen, provider.Options{
		Issuer:       *issuer,
		ClientID:     *clientID,
		ClientSecret: *clientSecret,
		Consent:      !*noConsent,
		RequirePKCE:  *requirePKCE,
		Claims: provider.Claims{
			Subject:       *subject,
			Email:         *email,
			Name:          *name,
			Username:      *username,
			EmailVerified: *verified,
			Groups:        splitList(*groups),
		},
	})
	if err != nil {
		log.Fatalf("testidp: %v", err)
	}
	if *sabotage != "" {
		idp.BreakDocument(*sabotage)
	}

	fmt.Printf("testidp listening on %s\n", addr)
	fmt.Printf("  issuer      %s\n", idp.Issuer())
	fmt.Printf("  discovery   %s/.well-known/openid-configuration\n", idp.Issuer())
	fmt.Printf("  jwks        %s/jwks\n", idp.Issuer())
	fmt.Printf("  client      %s (secret set: %t)\n", *clientID, *clientSecret != "")
	fmt.Printf("  asserts     sub=%s email=%s email_verified=%t groups=%v\n",
		*subject, *email, *verified, splitList(*groups))
	fmt.Printf("  consent     %t\n", !*noConsent)
	fmt.Println()
	fmt.Println(`walk it:
  jar=$(mktemp)
  curl -s -c $jar -b $jar -L "` + idp.Issuer() + `/authorize?client_id=` + *clientID + `&response_type=code&redirect_uri=` + idp.Issuer() + `/callback&state=state-1&scope=openid%20email%20profile&nonce=nonce-1&code_challenge_method=S256&code_challenge=QRy8Cz78gnbyjv7z_qjaOBueDRH805HXtQO9iDyW3aU"
  curl -s -b $jar -L "` + idp.Issuer() + `/consent?approve=state-1"      # -> /callback?code=code-state-1
  curl -s -X POST "` + idp.Issuer() + `/token" \
    -d grant_type=authorization_code -d code=code-state-1 \
    -d client_id=` + *clientID + ` -d client_secret=` + *clientSecret + ` \
    -d redirect_uri=` + idp.Issuer() + `/callback \
    -d code_verifier=dBjJhvNaTfJmYEUPuDaFHjFDBUC2QnKe9LZDBcQKmvk
  curl -s -H "Authorization: Bearer <access_token>" "` + idp.Issuer() + `/userinfo"`)
	fmt.Println()
	fmt.Println("Ctrl-C to stop.")

	// The provider serves until the process is stopped; nothing here owns a
	// shutdown path beyond the signal, which is the right amount of machinery for
	// a test fixture.
	sig := make(chan int)
	<-sig
}

func splitList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
