#!/usr/bin/env bash
# Checks that a Teleport source tree still has the hook anchors and upstream
# APIs the TAI OIDC port depends on. Run it in the TARGET tree before porting
# (e.g. a fresh branch from v18.11.10). Every FAIL/WARN line names what to
# adapt; see TAI_OIDC_IMPLEMENTATION.md §5.
#
# Usage: bash check-upstream.sh [TELEPORT_ROOT]
set -uo pipefail

ROOT="${1:-$(git rev-parse --show-toplevel)}"
cd "$ROOT" || exit 2

fails=0
warns=0

if [ -d lib/oidcsso ] || grep -qF 'h.bindOIDCLoginHandlers()' lib/web/apiserver.go 2>/dev/null; then
	echo "NOTE  this tree already contains the TAI OIDC port (lib/oidcsso or bindOIDCLoginHandlers found)."
	echo "      Run this check on a clean upstream checkout instead."
	exit 3
fi

# need FILE PATTERN DESCRIPTION: PATTERN (fixed string) must be present.
need() {
	if grep -qF -- "$2" "$1" 2>/dev/null; then
		printf 'PASS  %-45s %s\n' "$1" "$3"
	else
		printf 'FAIL  %-45s %s (missing: %s)\n' "$1" "$3" "$2"
		fails=$((fails + 1))
	fi
}

# absent FILE PATTERN DESCRIPTION: PATTERN must NOT be present (already
# ported, or upstream started shipping its own implementation).
absent() {
	if grep -qF -- "$2" "$1" 2>/dev/null; then
		printf 'WARN  %-45s %s (found: %s)\n' "$1" "$3" "$2"
		warns=$((warns + 1))
	else
		printf 'PASS  %-45s %s\n' "$1" "$3"
	fi
}

echo "== Hook anchors (hooks.patch) =="
need lib/auth/oidc.go 'type OIDCService interface' 'OIDCService interface'
need lib/auth/oidc.go 'errOIDCNotImplemented' 'OSS stub returning enterprise-only error'
need lib/auth/oidc.go 'func (a *Server) CreateOIDCAuthRequest(' 'Server.CreateOIDCAuthRequest delegate'
need lib/auth/oidc.go 'func (a *Server) CreateOIDCAuthRequestForMFA(' 'Server.CreateOIDCAuthRequestForMFA delegate'
need lib/auth/oidc.go 'func (a *Server) ValidateOIDCAuthCallback(' 'Server.ValidateOIDCAuthCallback delegate'
need lib/auth/auth.go 'oidcAuthService OIDCService' 'Server.oidcAuthService field'
need lib/auth/auth.go 'func (a *Server) SetOIDCService(' 'enterprise injection point'
need lib/auth/auth.go 'closeCtx ' 'Server.closeCtx (provider cache lifetime)'
need lib/auth/apiserver.go '"/:version/github/requests/validate"' 'auth HTTP SSO route block'
absent lib/auth/apiserver.go '"/:version/oidc/requests/validate"' 'OIDC validate route not yet registered'
need lib/modules/modules.go 'func (p *defaultModules) Features() Features' 'OSS features'
need lib/modules/modules.go 'entitlements.K8s:' 'OSS entitlement map anchor'
need lib/web/apiserver.go 'h.POST("/webapi/github/login/console"' 'GitHub SSO route block'
absent lib/web/apiserver.go '"/webapi/oidc/login/web"' 'OIDC web routes not yet registered'
absent lib/auth/oidc.go 'getOIDCService' 'built-in OIDC not yet wired'
need tool/tctl/sso/configure/oidc.go 'var oidcPresets = oidcPresetList(' 'tctl preset list'
need tool/tctl/sso/configure/oidc.go 'sub.Flag("acr"' 'tctl flag anchor'

echo
echo "== Enterprise gating (all read the OIDC entitlement) =="
n=$(grep -c 'GetEntitlement(entitlements.OIDC)' lib/auth/auth_with_roles.go 2>/dev/null || echo 0)
if [ "$n" -ge 1 ]; then
	printf 'PASS  %-45s %s gating sites (v18.10.0 had 4)\n' lib/auth/auth_with_roles.go "$n"
else
	printf 'WARN  %-45s no entitlements.OIDC gating found; check how OIDC is gated\n' lib/auth/auth_with_roles.go
	warns=$((warns + 1))
fi
other=$(grep -rln 'entitlements.OIDC' lib tool --include='*.go' 2>/dev/null | grep -v '_test.go' | grep -v -e lib/auth/auth_with_roles.go -e lib/modules/ -e lib/web/ || true)
if [ -n "$other" ]; then
	printf 'WARN  new entitlements.OIDC references outside known files:\n%s\n' "$other"
	warns=$((warns + 1))
fi

echo
echo "== Upstream APIs used by the new files =="
need api/types/oidc.go 'GetUsernameClaim() string' 'OIDCConnector.GetUsernameClaim'
need api/types/oidc.go 'GetTraitMappings() TraitMappingSet' 'OIDCConnector.GetTraitMappings'
need api/types/oidc.go 'GetMaxAge() (time.Duration, bool)' 'OIDCConnector.GetMaxAge'
need api/types/oidc.go 'IsPKCEEnabled() bool' 'OIDCConnector.IsPKCEEnabled'
need api/types/oidc.go 'GetClientRedirectSettings()' 'OIDCConnector.GetClientRedirectSettings'
need api/types/oidc.go 'GetAllowUnverifiedEmail() bool' 'OIDCConnector.GetAllowUnverifiedEmail'
need api/types/types.pb.go 'PkceVerifier string' 'OIDCAuthRequest.PkceVerifier'
need api/types/types.pb.go 'LoginHint string' 'OIDCAuthRequest.LoginHint'
need api/types/types.pb.go 'OIDCTraitsFromClaims' 'SSODiagnosticInfo OIDC fields'
need lib/services/oidc.go 'func GetRedirectURL(' 'services.GetRedirectURL'
need lib/services/traits.go 'func TraitsToRoles(' 'services.TraitsToRoles'
need lib/client/sso/redirector.go 'func ValidateClientRedirect(' 'sso.ValidateClientRedirect'
need lib/auth/github.go 'func (a *Server) makeGithubAuthResponse(' 'GitHub response template'
need lib/auth/github.go 'type CreateUserParams struct' 'auth.CreateUserParams'
need lib/auth/github.go 'InvalidClientRedirectErrorMessage' 'InvalidClientRedirectErrorMessage'
need lib/auth/sso_diag_context.go 'func NewSSODiagContext(' 'SSO diagnostics'
need lib/auth/authclient/http_client.go 'c.Endpoint("oidc", "requests", "validate")' 'proxy -> auth validate path'
need lib/auth/authclient/http_client.go 'type OIDCAuthRawResponse struct' 'OIDCAuthRawResponse'
need lib/auth/authclient/http_client.go 'type ValidateOIDCAuthCallbackReq struct' 'ValidateOIDCAuthCallbackReq'
need lib/web/apiserver.go 'func ParseSSORequestParams(' 'web.ParseSSORequestParams'
need lib/web/apiserver.go 'func SSOSetWebSessionAndRedirectURL(' 'web.SSOSetWebSessionAndRedirectURL'
need lib/web/apiserver.go 'func (h *Handler) githubCallback(' 'GitHub web handler template'
need lib/client/weblogin.go 'PKCEVerifier string' 'SSOLoginConsoleReq.PKCEVerifier'
need api/client/webclient/webconfig.go '/v1/webapi/oidc/login/web' 'frontend OIDC button URL'
need lib/client/sso.go '"webapi", connectorType, "login", "console"' 'tsh console login path'
need go.mod 'github.com/coreos/go-oidc/v3' 'go-oidc dependency'

echo
echo "== Summary: $fails FAIL, $warns WARN =="
[ "$fails" -eq 0 ]
