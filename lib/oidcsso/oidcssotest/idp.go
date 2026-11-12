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

// Package oidcssotest provides a fake OIDC identity provider, modeled on TAI,
// for testing the built-in OIDC connector.
package oidcssotest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"

	"github.com/gravitational/teleport/api/types"
)

const (
	// ClientID is the client ID the fake IdP issues ID tokens for.
	ClientID = "teleport-client"
	// ClientSecret is the client secret used by test connectors.
	ClientSecret = "teleport-secret"
	// Code is the only authorization code accepted by the token endpoint.
	Code = "auth-code"
	// RedirectURL is the redirect URL used by test connectors.
	RedirectURL = "https://proxy.example.com/v1/webapi/oidc/callback"
	// Subject is the sub claim of issued ID tokens.
	Subject = "user123"
	// UserName is the default user_name claim of issued ID tokens.
	UserName = "zhangsan"

	keyID = "test-key"
)

// IdP is a fake OIDC identity provider serving discovery, JWKS and a token
// endpoint that returns a preconfigured ID token.
type IdP struct {
	srv    *httptest.Server
	signer jose.Signer

	mu        sync.Mutex
	idToken   string
	tokenForm url.Values
}

// Option configures the fake IdP.
type Option func(*options)

type options struct {
	signingKey, publishedKey *rsa.PrivateKey
}

// WithMismatchedKeys makes the IdP sign ID tokens with a key that differs from
// the one published in its JWKS.
func WithMismatchedKeys(t *testing.T) Option {
	return func(o *options) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		o.signingKey = key
	}
}

// New starts a fake IdP that is shut down when the test ends.
func New(t *testing.T, opts ...Option) *IdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	o := options{signingKey: key, publishedKey: key}
	for _, opt := range opts {
		opt(&o)
	}

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: o.signingKey, KeyID: keyID}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	require.NoError(t, err)

	idp := &IdP{signer: signer}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                                idp.URL(),
			"authorization_endpoint":                idp.URL() + "/authorize",
			"token_endpoint":                        idp.URL() + "/token",
			"jwks_uri":                              idp.URL() + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       &o.publishedKey.PublicKey,
			KeyID:     keyID,
			Algorithm: string(jose.RS256),
			Use:       "sig",
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		idp.mu.Lock()
		idp.tokenForm = r.PostForm
		idToken := idp.idToken
		idp.mu.Unlock()

		if r.PostForm.Get("code") != Code {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": "access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     idToken,
		})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// URL returns the issuer URL.
func (p *IdP) URL() string {
	return p.srv.URL
}

// ConnectorSpec returns a TAI-style connector spec pointing at this IdP:
// user_name is the username claim and every user gets the access role.
func (p *IdP) ConnectorSpec() types.OIDCConnectorSpecV3 {
	return types.OIDCConnectorSpecV3{
		IssuerURL:     p.URL(),
		ClientID:      ClientID,
		ClientSecret:  ClientSecret,
		RedirectURLs:  []string{RedirectURL},
		Scope:         []string{"openid", "profile"},
		UsernameClaim: "user_name",
		Display:       "TAI",
		ClaimsToRoles: []types.ClaimMapping{
			{Claim: "user_name", Value: "*", Roles: []string{"access"}},
		},
	}
}

// Claims returns valid TAI-style ID token claims for the given nonce.
func (p *IdP) Claims(nonce string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":       p.URL(),
		"aud":       ClientID,
		"sub":       Subject,
		"user_name": UserName,
		"name":      "张三",
		"iat":       now.Unix(),
		"exp":       now.Add(time.Hour).Unix(),
		"nonce":     nonce,
	}
}

// Sign signs claims as an ID token.
func (p *IdP) Sign(t *testing.T, claims map[string]any) string {
	token, err := josejwt.Signed(p.signer).Claims(claims).Serialize()
	require.NoError(t, err)
	return token
}

// SetIDToken sets the ID token returned by the token endpoint.
func (p *IdP) SetIDToken(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.idToken = token
}

// LastTokenForm returns the form of the last token request.
func (p *IdP) LastTokenForm() url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tokenForm
}

// PrepareLogin simulates the user authenticating at the IdP: given the
// authorization URL produced by Teleport, it makes the token endpoint return
// an ID token bound to the request's nonce and returns the state token.
// mutate, if set, can alter the claims before signing.
func (p *IdP) PrepareLogin(t *testing.T, authURL string, mutate func(claims map[string]any)) (state string) {
	u, err := url.Parse(authURL)
	require.NoError(t, err)
	require.Equal(t, p.URL()+"/authorize", u.Scheme+"://"+u.Host+u.Path, "unexpected authorization endpoint")
	claims := p.Claims(u.Query().Get("nonce"))
	if mutate != nil {
		mutate(claims)
	}
	p.SetIDToken(p.Sign(t, claims))
	return u.Query().Get("state")
}
