/*
 * Teleport
 * Copyright (C) 2023  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

// Package oidcsso implements the OIDC relying-party protocol used by the
// built-in OIDC SSO connector: provider discovery, authorization URL
// construction, code exchange, ID token verification and claim mapping.
//
// The package is intentionally independent of the auth server so it can be
// reused unchanged across Teleport versions; lib/auth only wires it to
// storage, user management and certificate issuance.
package oidcsso

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"golang.org/x/oauth2"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"
)

const (
	// ProviderCacheTTL is how long discovered provider metadata is cached
	// before the discovery document is fetched again.
	ProviderCacheTTL = 10 * time.Minute

	// DefaultUsernameClaim is the claim used as the Teleport username when
	// the connector does not set username_claim.
	DefaultUsernameClaim = "email"

	// authTimeLeeway is the clock skew tolerated when checking the auth_time
	// claim against the connector's max_age.
	authTimeLeeway = time.Minute
)

// ErrNoRoles is returned when none of the connector's claims_to_roles
// mappings matched the claims of the authenticated user.
var ErrNoRoles = trace.AccessDenied("unable to map claims to any role; check the connector's claims_to_roles configuration")

// newHTTPClient returns the HTTP client used to talk to identity providers.
func newHTTPClient() (*http.Client, error) {
	httpClient, err := defaults.HTTPClient()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	httpClient.Timeout = defaults.HTTPRequestTimeout
	return httpClient, nil
}

// ProviderCache caches discovered OIDC providers by issuer URL, avoiding a
// discovery round-trip on every login.
type ProviderCache struct {
	cache *utils.FnCache
}

// NewProviderCache creates a provider cache. ctx bounds the lifetime of the
// cache and of the JWKS refreshes performed by cached providers.
func NewProviderCache(ctx context.Context, clock clockwork.Clock) (*ProviderCache, error) {
	cache, err := utils.NewFnCache(utils.FnCacheConfig{
		TTL:         ProviderCacheTTL,
		Clock:       clock,
		Context:     ctx,
		ReloadOnErr: true,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &ProviderCache{cache: cache}, nil
}

// Get returns the provider for issuerURL, performing discovery if needed.
func (c *ProviderCache) Get(ctx context.Context, issuerURL string) (*oidc.Provider, error) {
	provider, err := utils.FnCacheGet(ctx, c.cache, issuerURL, func(cacheCtx context.Context) (*oidc.Provider, error) {
		httpClient, err := newHTTPClient()
		if err != nil {
			return nil, trace.Wrap(err)
		}
		// The cache context outlives individual requests; go-oidc keeps the
		// context to lazily fetch and rotate the JWKS.
		provider, err := oidc.NewProvider(oidc.ClientContext(cacheCtx, httpClient), issuerURL)
		if err != nil {
			return nil, trace.Wrap(err, "failed to discover OIDC provider %q", issuerURL)
		}
		return provider, nil
	})
	return provider, trace.Wrap(err)
}

// OAuth2Config returns the OAuth2 client configuration for connector. The
// openid scope is always requested; email is added when no other scope is
// configured.
func OAuth2Config(connector types.OIDCConnector, provider *oidc.Provider, redirectURL string) oauth2.Config {
	scopes := []string{oidc.ScopeOpenID}
	for _, scope := range connector.GetScope() {
		if !slices.Contains(scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	if len(scopes) == 1 {
		scopes = append(scopes, "email")
	}
	return oauth2.Config{
		ClientID:     connector.GetClientID(),
		ClientSecret: connector.GetClientSecret(),
		RedirectURL:  redirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       scopes,
	}
}

// AuthCodeParams are the per-request inputs of the authorization URL.
type AuthCodeParams struct {
	// State is the opaque state token. It doubles as the nonce, binding the
	// ID token to the auth request.
	State string
	// LoginHint is an optional login_hint passed to the IdP.
	LoginHint string
	// PKCEVerifier is an optional client-provided PKCE verifier. It is only
	// used when the connector enables PKCE.
	PKCEVerifier string
}

// AuthCodeURL returns the IdP authorization URL and the PKCE verifier that
// must be stored with the auth request (empty when PKCE is disabled).
func AuthCodeURL(connector types.OIDCConnector, provider *oidc.Provider, redirectURL string, p AuthCodeParams) (authURL, pkceVerifier string) {
	opts := []oauth2.AuthCodeOption{oidc.Nonce(p.State)}
	if prompt := connector.GetPrompt(); prompt != "" {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", prompt))
	}
	if acr := connector.GetACR(); acr != "" {
		opts = append(opts, oauth2.SetAuthURLParam("acr_values", acr))
	}
	if maxAge, ok := connector.GetMaxAge(); ok {
		opts = append(opts, oauth2.SetAuthURLParam("max_age", strconv.Itoa(int(maxAge.Seconds()))))
	}
	if p.LoginHint != "" {
		opts = append(opts, oauth2.SetAuthURLParam("login_hint", p.LoginHint))
	}
	if connector.IsPKCEEnabled() {
		pkceVerifier = p.PKCEVerifier
		if pkceVerifier == "" {
			pkceVerifier = oauth2.GenerateVerifier()
		}
		opts = append(opts, oauth2.S256ChallengeOption(pkceVerifier))
	}
	config := OAuth2Config(connector, provider, redirectURL)
	return config.AuthCodeURL(p.State, opts...), pkceVerifier
}

// VerifyParams are the inputs needed to complete a login.
type VerifyParams struct {
	// Code is the authorization code returned by the IdP.
	Code string
	// PKCEVerifier is the verifier stored with the auth request, if any.
	PKCEVerifier string
	// Nonce is the expected nonce (the state token).
	Nonce string
	// Now is the current time, used for max_age checks.
	Now time.Time
}

// Result is a verified OIDC login.
type Result struct {
	// IDToken is the verified ID token.
	IDToken *oidc.IDToken
	// Claims are the claims of the verified ID token.
	Claims map[string]any
}

// ExchangeAndVerify exchanges the authorization code for tokens and fully
// verifies the ID token. Signature, issuer, audience and expiry are enforced
// by go-oidc; nonce, max_age, acr and email_verified are enforced here.
func ExchangeAndVerify(ctx context.Context, connector types.OIDCConnector, provider *oidc.Provider, redirectURL string, p VerifyParams) (*Result, error) {
	config := OAuth2Config(connector, provider, redirectURL)

	var exchangeOpts []oauth2.AuthCodeOption
	if p.PKCEVerifier != "" {
		exchangeOpts = append(exchangeOpts, oauth2.VerifierOption(p.PKCEVerifier))
	}

	httpClient, err := newHTTPClient()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	token, err := config.Exchange(context.WithValue(ctx, oauth2.HTTPClient, httpClient), p.Code, exchangeOpts...)
	if err != nil {
		return nil, trace.WithUserMessage(trace.Wrap(err), "Unable to exchange authorization code with the identity provider.")
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, trace.BadParameter("identity provider did not return an ID token; make sure the \"openid\" scope is requested")
	}

	verifier := provider.Verifier(&oidc.Config{ClientID: connector.GetClientID()})
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, trace.WithUserMessage(trace.AccessDenied("failed to verify ID token: %v", err), "Failed to verify ID token from the identity provider.")
	}

	// Some providers do not echo the nonce back, so only a mismatching nonce
	// is rejected.
	if idToken.Nonce != "" && idToken.Nonce != p.Nonce {
		return nil, trace.AccessDenied("ID token nonce does not match the auth request")
	}

	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return nil, trace.Wrap(err, "failed to parse ID token claims")
	}

	if err := CheckAuthTime(connector, claims, p.Now); err != nil {
		return nil, trace.Wrap(err)
	}
	if err := CheckACR(connector, claims); err != nil {
		return nil, trace.Wrap(err)
	}

	return &Result{IDToken: idToken, Claims: claims}, nil
}

// CheckAuthTime enforces the connector's max_age using the auth_time claim.
func CheckAuthTime(connector types.OIDCConnector, claims map[string]any, now time.Time) error {
	maxAge, ok := connector.GetMaxAge()
	if !ok {
		return nil
	}
	authTimeRaw, ok := claims["auth_time"].(float64)
	if !ok {
		return trace.AccessDenied("connector has max_age set but the ID token does not contain a valid auth_time claim")
	}
	authTime := time.Unix(int64(authTimeRaw), 0)
	if now.Sub(authTime) > maxAge+authTimeLeeway {
		return trace.AccessDenied("user authenticated at %v, which exceeds the connector's max_age of %v", authTime.UTC(), maxAge)
	}
	return nil
}

// CheckACR verifies that the acr claim matches one of the requested
// acr_values, if any were requested.
func CheckACR(connector types.OIDCConnector, claims map[string]any) error {
	acrValues := connector.GetACR()
	if acrValues == "" {
		return nil
	}
	acr, _ := claims["acr"].(string)
	if !slices.Contains(strings.Fields(acrValues), acr) {
		return trace.AccessDenied("ID token acr claim %q does not match any of the requested acr_values %q", acr, acrValues)
	}
	return nil
}

// CheckEmailVerified rejects identities whose email is explicitly unverified,
// unless the connector allows unverified emails.
func CheckEmailVerified(connector types.OIDCConnector, claims map[string]any) error {
	if connector.GetAllowUnverifiedEmail() {
		return nil
	}
	verified, ok := claims["email_verified"]
	if !ok {
		return nil
	}
	switch v := verified.(type) {
	case bool:
		if !v {
			return trace.AccessDenied("email not verified by OIDC provider")
		}
	case string:
		if v != "true" {
			return trace.AccessDenied("email not verified by OIDC provider")
		}
	}
	return nil
}

// UsernameFromClaims extracts the Teleport username from the claim configured
// by the connector's username_claim (email by default). The value is trimmed
// and lower-cased.
func UsernameFromClaims(connector types.OIDCConnector, claims map[string]any) (string, error) {
	usernameClaim := connector.GetUsernameClaim()
	if usernameClaim == "" {
		usernameClaim = DefaultUsernameClaim
	}
	raw, ok := claims[usernameClaim]
	if !ok {
		return "", trace.BadParameter("ID token is missing the %q claim configured as username_claim", usernameClaim)
	}
	username, ok := raw.(string)
	if !ok {
		return "", trace.BadParameter("claim %q configured as username_claim must be a string, got %T", usernameClaim, raw)
	}
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return "", trace.BadParameter("claim %q configured as username_claim is empty", usernameClaim)
	}
	if len(username) > teleport.MaxUsernameLength {
		return "", trace.BadParameter("claim %q configured as username_claim exceeds the maximum username length of %d", usernameClaim, teleport.MaxUsernameLength)
	}
	return username, nil
}

// IdentityFromClaims returns the OIDC identity recorded in SSO diagnostics.
func IdentityFromClaims(idToken *oidc.IDToken, claims map[string]any) types.OIDCIdentity {
	identity := types.OIDCIdentity{
		ID:        idToken.Subject,
		ExpiresAt: idToken.Expiry,
	}
	identity.Email, _ = claims["email"].(string)
	identity.Name, _ = claims["name"].(string)
	return identity
}

// ClaimsToTraits converts ID token claims to user traits. Scalar claims become
// single-valued traits, arrays become multi-valued traits and nested objects
// are ignored.
func ClaimsToTraits(claims map[string]any) map[string][]string {
	traits := make(map[string][]string, len(claims))
	for name, value := range claims {
		switch v := value.(type) {
		case []any:
			var values []string
			for _, item := range v {
				if s, ok := scalarToString(item); ok {
					values = append(values, s)
				}
			}
			if len(values) > 0 {
				traits[name] = values
			}
		default:
			if s, ok := scalarToString(v); ok {
				traits[name] = []string{s}
			}
		}
	}
	return traits
}

func scalarToString(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	default:
		return "", false
	}
}

// MapRoles maps traits to roles using the connector's claims_to_roles, with
// the same semantics as SAML and GitHub connectors (globs, ^regexp$ and $1
// expansion). It returns ErrNoRoles if nothing matched.
func MapRoles(connector types.OIDCConnector, traits map[string][]string) (roles, warnings []string, err error) {
	warnings, roles = services.TraitsToRoles(connector.GetTraitMappings(), traits)
	if len(roles) == 0 {
		return nil, warnings, trace.Wrap(ErrNoRoles)
	}
	return roles, warnings, nil
}
