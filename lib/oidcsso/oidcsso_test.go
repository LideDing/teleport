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

package oidcsso_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/oidcsso"
	"github.com/gravitational/teleport/lib/oidcsso/oidcssotest"
)

func newConnector(t *testing.T, modify func(spec *types.OIDCConnectorSpecV3)) types.OIDCConnector {
	spec := types.OIDCConnectorSpecV3{
		IssuerURL:     "https://tai.example.com",
		ClientID:      oidcssotest.ClientID,
		ClientSecret:  oidcssotest.ClientSecret,
		RedirectURLs:  []string{oidcssotest.RedirectURL},
		UsernameClaim: "user_name",
		ClaimsToRoles: []types.ClaimMapping{{Claim: "user_name", Value: "*", Roles: []string{"access"}}},
	}
	if modify != nil {
		modify(&spec)
	}
	conn, err := types.NewOIDCConnector("tai", spec)
	require.NoError(t, err)
	return conn
}

func TestUsernameFromClaims(t *testing.T) {
	t.Parallel()
	tai := newConnector(t, nil)
	email := newConnector(t, func(spec *types.OIDCConnectorSpecV3) { spec.UsernameClaim = "" })

	tests := []struct {
		name      string
		connector types.OIDCConnector
		claims    map[string]any
		want      string
		wantErr   bool
	}{
		{name: "user_name", connector: tai, claims: map[string]any{"user_name": "zhangsan"}, want: "zhangsan"},
		{name: "normalized", connector: tai, claims: map[string]any{"user_name": "  ZhangSan "}, want: "zhangsan"},
		{name: "default email", connector: email, claims: map[string]any{"email": "a@b.c"}, want: "a@b.c"},
		{name: "missing", connector: tai, claims: map[string]any{"email": "a@b.c"}, wantErr: true},
		{name: "empty", connector: tai, claims: map[string]any{"user_name": "  "}, wantErr: true},
		{name: "not a string", connector: tai, claims: map[string]any{"user_name": []any{"a"}}, wantErr: true},
		{name: "too long", connector: tai, claims: map[string]any{"user_name": strings.Repeat("a", 1001)}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := oidcsso.UsernameFromClaims(tc.connector, tc.claims)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestClaimsToTraits(t *testing.T) {
	t.Parallel()
	traits := oidcsso.ClaimsToTraits(map[string]any{
		"user_name": "zhangsan",
		"groups":    []any{"dev", "ops", map[string]any{"nested": true}},
		"admin":     true,
		"level":     float64(3),
		"address":   map[string]any{"city": "x"},
		"empty":     []any{},
	})
	require.Equal(t, map[string][]string{
		"user_name": {"zhangsan"},
		"groups":    {"dev", "ops"},
		"admin":     {"true"},
		"level":     {"3"},
	}, traits)
}

func TestMapRoles(t *testing.T) {
	t.Parallel()
	conn := newConnector(t, func(spec *types.OIDCConnectorSpecV3) {
		spec.ClaimsToRoles = []types.ClaimMapping{
			{Claim: "user_name", Value: "*", Roles: []string{"access"}},
			{Claim: "groups", Value: "admin-*", Roles: []string{"editor"}},
			{Claim: "groups", Value: "^team-(.*)$", Roles: []string{"team-$1"}},
		}
	})

	roles, _, err := oidcsso.MapRoles(conn, map[string][]string{
		"user_name": {"zhangsan"},
		"groups":    {"admin-ops", "team-db"},
	})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"access", "editor", "team-db"}, roles)

	_, _, err = oidcsso.MapRoles(conn, map[string][]string{"other": {"x"}})
	require.ErrorIs(t, err, oidcsso.ErrNoRoles)
}

func TestCheckEmailVerified(t *testing.T) {
	t.Parallel()
	strict := newConnector(t, nil)
	lax := newConnector(t, func(spec *types.OIDCConnectorSpecV3) { spec.AllowUnverifiedEmail = true })

	require.NoError(t, oidcsso.CheckEmailVerified(strict, map[string]any{}))
	require.NoError(t, oidcsso.CheckEmailVerified(strict, map[string]any{"email_verified": true}))
	require.True(t, trace.IsAccessDenied(oidcsso.CheckEmailVerified(strict, map[string]any{"email_verified": false})))
	require.True(t, trace.IsAccessDenied(oidcsso.CheckEmailVerified(strict, map[string]any{"email_verified": "false"})))
	require.NoError(t, oidcsso.CheckEmailVerified(lax, map[string]any{"email_verified": false}))
}

func TestCheckAuthTimeAndACR(t *testing.T) {
	t.Parallel()
	now := time.Now()
	conn := newConnector(t, func(spec *types.OIDCConnectorSpecV3) {
		spec.MaxAge = &types.MaxAge{Value: types.Duration(time.Hour)}
		spec.ACR = "gold silver"
	})

	require.NoError(t, oidcsso.CheckAuthTime(conn, map[string]any{"auth_time": float64(now.Add(-30 * time.Minute).Unix())}, now))
	require.Error(t, oidcsso.CheckAuthTime(conn, map[string]any{"auth_time": float64(now.Add(-2 * time.Hour).Unix())}, now))
	require.Error(t, oidcsso.CheckAuthTime(conn, map[string]any{}, now))
	require.NoError(t, oidcsso.CheckAuthTime(newConnector(t, nil), map[string]any{}, now))

	require.NoError(t, oidcsso.CheckACR(conn, map[string]any{"acr": "silver"}))
	require.Error(t, oidcsso.CheckACR(conn, map[string]any{"acr": "bronze"}))
	require.NoError(t, oidcsso.CheckACR(newConnector(t, nil), map[string]any{}))
}

func TestAuthCodeURLAndExchange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	idp := oidcssotest.New(t)
	conn, err := types.NewOIDCConnector("tai", idp.ConnectorSpec())
	require.NoError(t, err)

	cache, err := oidcsso.NewProviderCache(ctx, clockwork.NewRealClock())
	require.NoError(t, err)
	provider, err := cache.Get(ctx, idp.URL())
	require.NoError(t, err)

	authURL, verifier := oidcsso.AuthCodeURL(conn, provider, oidcssotest.RedirectURL, oidcsso.AuthCodeParams{State: "state123"})
	require.Empty(t, verifier, "PKCE is disabled")
	u, err := url.Parse(authURL)
	require.NoError(t, err)
	require.Equal(t, "state123", u.Query().Get("nonce"))

	state := idp.PrepareLogin(t, authURL, nil)
	require.Equal(t, "state123", state)

	result, err := oidcsso.ExchangeAndVerify(ctx, conn, provider, oidcssotest.RedirectURL, oidcsso.VerifyParams{
		Code:  oidcssotest.Code,
		Nonce: state,
		Now:   time.Now(),
	})
	require.NoError(t, err)
	require.Equal(t, oidcssotest.Subject, result.IDToken.Subject)
	require.Equal(t, oidcssotest.UserName, result.Claims["user_name"])

	identity := oidcsso.IdentityFromClaims(result.IDToken, result.Claims)
	require.Equal(t, oidcssotest.Subject, identity.ID)
	require.Equal(t, "张三", identity.Name)
}
