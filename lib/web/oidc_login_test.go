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

package web

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/api/utils/keys"
	"github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/cryptosuites"
	"github.com/gravitational/teleport/lib/httplib/csrf"
	"github.com/gravitational/teleport/lib/oidcsso/oidcssotest"
	"github.com/gravitational/teleport/lib/secret"
	"github.com/gravitational/teleport/lib/web/app"
	websession "github.com/gravitational/teleport/lib/web/session"
)

// setupOIDCWebPack starts a proxy and auth server with a "tai" OIDC connector
// pointing at a fake IdP.
func setupOIDCWebPack(t *testing.T) (*webPack, *oidcssotest.IdP) {
	ctx := context.Background()
	env := newWebPack(t, 1)
	idp := oidcssotest.New(t)

	connector, err := types.NewOIDCConnector("tai", idp.ConnectorSpec())
	require.NoError(t, err)
	_, err = env.server.Auth().UpsertOIDCConnector(ctx, connector)
	require.NoError(t, err)

	access, err := types.NewRole("access", types.RoleSpecV6{})
	require.NoError(t, err)
	_, err = env.server.Auth().CreateRole(ctx, access)
	require.NoError(t, err)

	return env, idp
}

func newTestSSHPublicKey(t *testing.T) ssh.PublicKey {
	sshKey, err := cryptosuites.GenerateKeyWithAlgorithm(cryptosuites.Ed25519)
	require.NoError(t, err)
	sshPub, err := ssh.NewPublicKey(sshKey.Public())
	require.NoError(t, err)
	return sshPub
}

// TestOIDCConsoleLogin exercises the tsh login flow through the proxy:
// POST /webapi/oidc/login/console followed by the IdP callback, which reaches
// the auth server through POST /:version/oidc/requests/validate.
func TestOIDCConsoleLogin(t *testing.T) {
	ctx := context.Background()
	env, idp := setupOIDCWebPack(t)
	clt := env.proxies[0].newClient(t)

	secretKey, err := secret.NewKey()
	require.NoError(t, err)

	sshPub := newTestSSHPublicKey(t)
	tlsKey, err := cryptosuites.GenerateKeyWithAlgorithm(cryptosuites.ECDSAP256)
	require.NoError(t, err)
	tlsPubBytes, err := keys.MarshalPublicKey(tlsKey.Public())
	require.NoError(t, err)

	loginResponse, err := clt.PostJSON(ctx, clt.Endpoint("webapi", "oidc", "login", "console"), client.SSOLoginConsoleReq{
		RedirectURL: (&url.URL{
			Scheme:   "http",
			Host:     "localhost",
			Path:     "callback",
			RawQuery: url.Values{"secret_key": []string{secretKey.String()}}.Encode(),
		}).String(),
		ConnectorID: "tai",
		UserPublicKeys: client.UserPublicKeys{
			SSHPubKey: ssh.MarshalAuthorizedKey(sshPub),
			TLSPubKey: tlsPubBytes,
		},
		CertTTL: time.Hour,
	})
	require.NoError(t, err)

	var consoleResp client.SSOLoginConsoleResponse
	require.NoError(t, json.Unmarshal(loginResponse.Bytes(), &consoleResp))
	state := idp.PrepareLogin(t, consoleResp.RedirectURL, nil)

	callbackResponse, err := clt.Get(ctx, clt.Endpoint("webapi", "oidc", "callback"), url.Values{
		"state": []string{state},
		"code":  []string{oidcssotest.Code},
	})
	require.NoError(t, err)

	loginResp := sshLoginResponseFromCallbackResponse(t, callbackResponse.Reader(), secretKey)
	require.Equal(t, oidcssotest.UserName, loginResp.Username)
	require.NotEmpty(t, loginResp.Cert)
	require.NotEmpty(t, loginResp.TLSCert)
	require.NotEmpty(t, loginResp.HostSigners)
}

// TestOIDCConsoleLoginBadRedirect checks that disallowed client redirect URLs
// are rejected with the dedicated error message.
func TestOIDCConsoleLoginBadRedirect(t *testing.T) {
	ctx := context.Background()
	env, _ := setupOIDCWebPack(t)
	clt := env.proxies[0].newClient(t)

	_, err := clt.PostJSON(ctx, clt.Endpoint("webapi", "oidc", "login", "console"), client.SSOLoginConsoleReq{
		RedirectURL: "https://evil.example.com/callback?secret_key=abc",
		ConnectorID: "tai",
		UserPublicKeys: client.UserPublicKeys{
			SSHPubKey: ssh.MarshalAuthorizedKey(newTestSSHPublicKey(t)),
		},
		CertTTL: time.Hour,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, SSOLoginFailureInvalidRedirect)
}

// TestOIDCWebLogin exercises the browser login flow: GET
// /v1/webapi/oidc/login/web redirects to the IdP and the callback sets a web
// session cookie.
func TestOIDCWebLogin(t *testing.T) {
	env, idp := setupOIDCWebPack(t)
	proxy := env.proxies[0]

	csrfCookie := &http.Cookie{Name: csrf.CookieName, Value: hex.EncodeToString(make([]byte, 32))}

	httpClient := proxy.web.Client()
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	loginURL := proxy.webURL
	loginURL.Path = "/v1/webapi/oidc/login/web"
	loginURL.RawQuery = "connector_id=tai&redirect_url=" + url.QueryEscape("/web/cluster/foo")
	req, err := http.NewRequest(http.MethodGet, loginURL.String(), nil)
	require.NoError(t, err)
	req.AddCookie(csrfCookie)
	resp, err := httpClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode)

	state := idp.PrepareLogin(t, resp.Header.Get("Location"), nil)

	callbackURL := proxy.webURL
	callbackURL.Path = "/v1/webapi/oidc/callback"
	callbackURL.RawQuery = url.Values{"state": {state}, "code": {oidcssotest.Code}}.Encode()
	req, err = http.NewRequest(http.MethodGet, callbackURL.String(), nil)
	require.NoError(t, err)
	req.AddCookie(csrfCookie)
	resp, err = httpClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	redirect, err := app.GetURLFromMetaRedirect(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "/web/cluster/foo", redirect)

	var sessionCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == websession.CookieName {
			sessionCookie = c
		}
	}
	require.NotNil(t, sessionCookie, "web session cookie must be set")
	require.NotEmpty(t, sessionCookie.Value)
}
