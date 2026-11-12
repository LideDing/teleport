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

package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"testing"
	"time"

	"github.com/gravitational/trace"
	"github.com/stretchr/testify/require"

	"github.com/gravitational/teleport/api/constants"
	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/auth/authclient"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/events/eventstest"
	"github.com/gravitational/teleport/lib/oidcsso/oidcssotest"
)

// tshClientRedirectURL is a client redirect URL accepted for tsh logins.
const tshClientRedirectURL = "http://127.0.0.1:12345/callback?secret_key=deadbeef"

type oidcTestEnv struct {
	a       *auth.Server
	idp     *oidcssotest.IdP
	emitter *eventstest.MockRecorderEmitter
}

// setupOIDCTest starts an auth server with a "tai" connector pointing at a
// fake IdP. modify can adjust the connector spec.
func setupOIDCTest(t *testing.T, modify func(spec *types.OIDCConnectorSpecV3), idpOpts ...oidcssotest.Option) *oidcTestEnv {
	ctx := context.Background()
	srv := newTestTLSServer(t)
	a := srv.Auth()

	emitter := &eventstest.MockRecorderEmitter{}
	a.SetEmitter(emitter)

	for _, name := range []string{"access", "editor"} {
		role, err := types.NewRole(name, types.RoleSpecV6{
			Allow: types.RoleConditions{Logins: []string{"{{internal.logins}}"}},
		})
		require.NoError(t, err)
		_, err = a.UpsertRole(ctx, role)
		require.NoError(t, err)
	}

	idp := oidcssotest.New(t, idpOpts...)
	spec := idp.ConnectorSpec()
	spec.ClaimsToRoles = append(spec.ClaimsToRoles, types.ClaimMapping{Claim: "groups", Value: "admin-*", Roles: []string{"editor"}})
	if modify != nil {
		modify(&spec)
	}
	conn, err := types.NewOIDCConnector("tai", spec)
	require.NoError(t, err)
	_, err = a.UpsertOIDCConnector(ctx, conn)
	require.NoError(t, err)

	return &oidcTestEnv{a: a, idp: idp, emitter: emitter}
}

// login runs a complete login, letting mutate customize the ID token claims.
func (e *oidcTestEnv) login(t *testing.T, req types.OIDCAuthRequest, mutate func(claims map[string]any)) (*authclient.OIDCAuthResponse, error) {
	if req.ConnectorID == "" {
		req.ConnectorID = "tai"
	}
	created, err := e.a.CreateOIDCAuthRequest(context.Background(), req)
	require.NoError(t, err)
	state := e.idp.PrepareLogin(t, created.RedirectURL, mutate)
	return e.a.ValidateOIDCAuthCallback(context.Background(), url.Values{
		"code":  {oidcssotest.Code},
		"state": {state},
	})
}

func webLoginRequest() types.OIDCAuthRequest {
	return types.OIDCAuthRequest{CreateWebSession: true, ClientRedirectURL: "/web"}
}

func TestOIDCCreateAuthRequest(t *testing.T) {
	t.Parallel()
	env := setupOIDCTest(t, nil)

	req := webLoginRequest()
	req.ConnectorID = "tai"
	req.LoginHint = "zhangsan"
	created, err := env.a.CreateOIDCAuthRequest(context.Background(), req)
	require.NoError(t, err)
	require.NotEmpty(t, created.StateToken)

	authURL, err := url.Parse(created.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, env.idp.URL()+"/authorize", authURL.Scheme+"://"+authURL.Host+authURL.Path)

	q := authURL.Query()
	require.Equal(t, "code", q.Get("response_type"))
	require.Equal(t, oidcssotest.ClientID, q.Get("client_id"))
	require.Equal(t, oidcssotest.RedirectURL, q.Get("redirect_uri"))
	require.Equal(t, "openid profile", q.Get("scope"))
	require.Equal(t, created.StateToken, q.Get("state"))
	require.Equal(t, created.StateToken, q.Get("nonce"))
	require.Equal(t, constants.OIDCPromptSelectAccount, q.Get("prompt"))
	require.Equal(t, "zhangsan", q.Get("login_hint"))
	require.Empty(t, q.Get("code_challenge"), "PKCE is disabled by default")

	stored, err := env.a.GetOIDCAuthRequest(context.Background(), created.StateToken)
	require.NoError(t, err)
	require.Equal(t, created.StateToken, stored.StateToken)
}

func TestOIDCCreateAuthRequestRejectsBadClientRedirect(t *testing.T) {
	t.Parallel()
	env := setupOIDCTest(t, nil)

	_, err := env.a.CreateOIDCAuthRequest(context.Background(), types.OIDCAuthRequest{
		ConnectorID:       "tai",
		ClientRedirectURL: "https://evil.example.com/callback",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), auth.InvalidClientRedirectErrorMessage)
}

func TestOIDCWebLogin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := setupOIDCTest(t, nil)

	req := webLoginRequest()
	req.CSRFToken = "csrf"
	resp, err := env.login(t, req, func(claims map[string]any) {
		// Usernames are normalized to lower case.
		claims["user_name"] = " ZhangSan "
		claims["groups"] = []any{"dev", "admin-ops"}
	})
	require.NoError(t, err)
	require.Equal(t, "zhangsan", resp.Username)
	require.NotNil(t, resp.Session)
	require.True(t, resp.Req.CreateWebSession)
	require.Equal(t, "csrf", resp.Req.CSRFToken)
	require.Equal(t, types.ExternalIdentity{ConnectorID: "tai", Username: "zhangsan", UserID: oidcssotest.Subject}, resp.Identity)

	user, err := env.a.GetUser(ctx, "zhangsan", false)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"access", "editor"}, user.GetRoles())
	traits := user.GetTraits()
	require.Equal(t, []string{" ZhangSan "}, traits["user_name"])
	require.Equal(t, []string{"dev", "admin-ops"}, traits["groups"])
	require.Equal(t, []string{"zhangsan"}, traits[constants.TraitLogins])
	require.Equal(t, constants.OIDC, user.GetCreatedBy().Connector.Type)
	require.Len(t, user.GetOIDCIdentities(), 1)

	login, ok := env.emitter.LastEvent().(*apievents.UserLogin)
	require.True(t, ok)
	require.Equal(t, events.UserSSOLoginCode, login.Code)
	require.Equal(t, events.LoginMethodOIDC, login.Method)
	require.Equal(t, "zhangsan", login.User)

	// A second login updates the existing user.
	resp, err = env.login(t, webLoginRequest(), nil)
	require.NoError(t, err)
	require.Equal(t, "zhangsan", resp.Username)
	user, err = env.a.GetUser(ctx, "zhangsan", false)
	require.NoError(t, err)
	require.Equal(t, []string{"access"}, user.GetRoles())
}

func TestOIDCLoginRejectsExistingLocalUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := setupOIDCTest(t, nil)

	local, err := types.NewUser(oidcssotest.UserName)
	require.NoError(t, err)
	local.SetRoles([]string{"access"})
	_, err = env.a.CreateUser(ctx, local)
	require.NoError(t, err)

	_, err = env.login(t, webLoginRequest(), nil)
	require.True(t, trace.IsAlreadyExists(err), "expected already exists, got %v", err)
}

func TestOIDCConsoleLogin(t *testing.T) {
	t.Parallel()
	env := setupOIDCTest(t, nil)
	_, sshPub, _, tlsPub := newSSHAndTLSKeyPairs(t)

	resp, err := env.login(t, types.OIDCAuthRequest{
		ClientRedirectURL: tshClientRedirectURL,
		SshPublicKey:      sshPub,
		TlsPublicKey:      tlsPub,
		CertTTL:           time.Hour,
	}, nil)
	require.NoError(t, err)
	require.Equal(t, oidcssotest.UserName, resp.Username)
	require.Nil(t, resp.Session)
	require.NotEmpty(t, resp.Cert)
	require.NotEmpty(t, resp.TLSCert)
	require.Len(t, resp.HostSigners, 1)
}

func TestOIDCPKCE(t *testing.T) {
	t.Parallel()
	env := setupOIDCTest(t, func(spec *types.OIDCConnectorSpecV3) {
		spec.PKCEMode = string(constants.OIDCPKCEModeEnabled)
	})

	req := webLoginRequest()
	req.ConnectorID = "tai"
	created, err := env.a.CreateOIDCAuthRequest(context.Background(), req)
	require.NoError(t, err)
	authURL, err := url.Parse(created.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, "S256", authURL.Query().Get("code_challenge_method"))
	challenge := authURL.Query().Get("code_challenge")
	require.NotEmpty(t, challenge)

	state := env.idp.PrepareLogin(t, created.RedirectURL, nil)
	_, err = env.a.ValidateOIDCAuthCallback(context.Background(), url.Values{
		"code":  {oidcssotest.Code},
		"state": {state},
	})
	require.NoError(t, err)

	verifier := env.idp.LastTokenForm().Get("code_verifier")
	require.NotEmpty(t, verifier)
	sum := sha256.Sum256([]byte(verifier))
	require.Equal(t, challenge, base64.RawURLEncoding.EncodeToString(sum[:]))
}

func TestOIDCLoginFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		modify    func(spec *types.OIDCConnectorSpecV3)
		idpOpts   func(t *testing.T) []oidcssotest.Option
		claims    func(claims map[string]any)
		assertErr func(t *testing.T, err error)
	}{
		{
			name:   "missing username claim",
			claims: func(c map[string]any) { delete(c, "user_name") },
		},
		{
			name:   "non-string username claim",
			claims: func(c map[string]any) { c["user_name"] = 42 },
		},
		{
			name: "no roles matched",
			modify: func(spec *types.OIDCConnectorSpecV3) {
				spec.ClaimsToRoles = []types.ClaimMapping{{Claim: "groups", Value: "admins", Roles: []string{"access"}}}
			},
			claims: func(c map[string]any) { c["groups"] = []any{"dev"} },
			assertErr: func(t *testing.T, err error) {
				require.True(t, trace.IsAccessDenied(err), "expected access denied, got %v", err)
			},
		},
		{
			name:   "wrong audience",
			claims: func(c map[string]any) { c["aud"] = "someone-else" },
		},
		{
			name:   "wrong issuer",
			claims: func(c map[string]any) { c["iss"] = "https://evil.example.com" },
		},
		{
			name:   "expired token",
			claims: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
		},
		{
			name:   "nonce mismatch",
			claims: func(c map[string]any) { c["nonce"] = "other" },
		},
		{
			name: "bad signature",
			idpOpts: func(t *testing.T) []oidcssotest.Option {
				return []oidcssotest.Option{oidcssotest.WithMismatchedKeys(t)}
			},
			assertErr: func(t *testing.T, err error) {
				require.ErrorContains(t, err, "failed to verify ID token")
			},
		},
		{
			name: "unverified email",
			modify: func(spec *types.OIDCConnectorSpecV3) {
				spec.UsernameClaim = ""
			},
			claims: func(c map[string]any) {
				c["email"] = "zhangsan@example.com"
				c["email_verified"] = false
			},
		},
		{
			name: "max_age exceeded",
			modify: func(spec *types.OIDCConnectorSpecV3) {
				spec.MaxAge = &types.MaxAge{Value: types.Duration(time.Hour)}
			},
			claims: func(c map[string]any) { c["auth_time"] = time.Now().Add(-3 * time.Hour).Unix() },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var idpOpts []oidcssotest.Option
			if tc.idpOpts != nil {
				idpOpts = tc.idpOpts(t)
			}
			env := setupOIDCTest(t, tc.modify, idpOpts...)
			_, err := env.login(t, webLoginRequest(), tc.claims)
			require.Error(t, err)
			if tc.assertErr != nil {
				tc.assertErr(t, err)
			}

			login, ok := env.emitter.LastEvent().(*apievents.UserLogin)
			require.True(t, ok)
			require.Equal(t, events.UserSSOLoginFailureCode, login.Code)
			require.False(t, login.Status.Success)

			_, err = env.a.GetUser(context.Background(), oidcssotest.UserName, false)
			require.True(t, trace.IsNotFound(err), "no user should be created, got %v", err)
		})
	}
}

func TestOIDCEmailUsername(t *testing.T) {
	t.Parallel()
	env := setupOIDCTest(t, func(spec *types.OIDCConnectorSpecV3) {
		spec.UsernameClaim = ""
	})

	resp, err := env.login(t, webLoginRequest(), func(c map[string]any) {
		c["email"] = "zhangsan@example.com"
		c["email_verified"] = true
	})
	require.NoError(t, err)
	require.Equal(t, "zhangsan@example.com", resp.Username)
}

func TestOIDCCallbackErrors(t *testing.T) {
	t.Parallel()
	env := setupOIDCTest(t, nil)
	ctx := context.Background()

	// IdP returned an error.
	_, err := env.a.ValidateOIDCAuthCallback(ctx, url.Values{
		"error":             {"access_denied"},
		"error_description": {"user cancelled"},
	})
	require.Error(t, err)

	// Unknown state.
	_, err = env.a.ValidateOIDCAuthCallback(ctx, url.Values{
		"code":  {oidcssotest.Code},
		"state": {"unknown"},
	})
	require.True(t, trace.IsNotFound(err), "expected not found, got %v", err)

	// Bad code.
	req := webLoginRequest()
	req.ConnectorID = "tai"
	created, err := env.a.CreateOIDCAuthRequest(ctx, req)
	require.NoError(t, err)
	state := env.idp.PrepareLogin(t, created.RedirectURL, nil)
	_, err = env.a.ValidateOIDCAuthCallback(ctx, url.Values{
		"code":  {"wrong-code"},
		"state": {state},
	})
	require.Error(t, err)
}

func TestOIDCSSOTestFlow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := setupOIDCTest(t, nil)

	spec := env.idp.ConnectorSpec()
	created, err := env.a.CreateOIDCAuthRequest(ctx, types.OIDCAuthRequest{
		ConnectorID:       "test-tai",
		ClientRedirectURL: tshClientRedirectURL,
		SSOTestFlow:       true,
		ConnectorSpec:     &spec,
		CertTTL:           time.Hour,
	})
	require.NoError(t, err)
	state := env.idp.PrepareLogin(t, created.RedirectURL, func(c map[string]any) {
		c["user_name"] = "lisi"
	})

	resp, err := env.a.ValidateOIDCAuthCallback(ctx, url.Values{
		"code":  {oidcssotest.Code},
		"state": {state},
	})
	require.NoError(t, err)
	require.Equal(t, "lisi", resp.Username)
	require.Empty(t, resp.Cert)

	// Test flow is a dry run and must not create users.
	_, err = env.a.GetUser(ctx, "lisi", false)
	require.True(t, trace.IsNotFound(err), "expected not found, got %v", err)

	login, ok := env.emitter.LastEvent().(*apievents.UserLogin)
	require.True(t, ok)
	require.Equal(t, events.UserSSOTestFlowLoginCode, login.Code)

	diag, err := env.a.GetSSODiagnosticInfo(ctx, types.KindOIDC, created.StateToken)
	require.NoError(t, err)
	require.True(t, diag.TestFlow)
	require.True(t, diag.Success)
	require.Equal(t, "lisi", diag.OIDCClaims["user_name"])
	require.Equal(t, oidcssotest.Subject, diag.OIDCIdentity.ID)
	require.Equal(t, []string{"access"}, diag.CreateUserParams.Roles)
	require.Equal(t, []string{"lisi"}, diag.OIDCTraitsFromClaims["user_name"])
}

// fakeOIDCService records calls to make sure an injected (enterprise)
// implementation takes precedence over the built-in one.
type fakeOIDCService struct {
	auth.OIDCService
	called bool
}

func (f *fakeOIDCService) CreateOIDCAuthRequest(ctx context.Context, req types.OIDCAuthRequest) (*types.OIDCAuthRequest, error) {
	f.called = true
	return &req, nil
}

func TestOIDCInjectedServiceTakesPrecedence(t *testing.T) {
	t.Parallel()
	srv := newTestTLSServer(t)
	svc := &fakeOIDCService{}
	srv.Auth().SetOIDCService(svc)

	_, err := srv.Auth().CreateOIDCAuthRequest(context.Background(), types.OIDCAuthRequest{ConnectorID: "anything"})
	require.NoError(t, err)
	require.True(t, svc.called)
}
