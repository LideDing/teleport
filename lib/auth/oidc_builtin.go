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

package auth

// This file wires the protocol implementation in lib/oidcsso into the auth
// server: auth request storage, user upsert, web sessions, certificates,
// audit events and SSO diagnostics. It is structured after github.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gravitational/trace"
	"github.com/julienschmidt/httprouter"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/constants"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/api/utils/keys/hardwarekey"
	"github.com/gravitational/teleport/lib/auth/authclient"
	"github.com/gravitational/teleport/lib/authz"
	"github.com/gravitational/teleport/lib/client/sso"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/httplib"
	"github.com/gravitational/teleport/lib/loginrule"
	"github.com/gravitational/teleport/lib/oidcsso"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"
)

// ErrOIDCNoRoles is returned when none of the connector's claims_to_roles
// mappings matched the claims of the authenticated user.
var ErrOIDCNoRoles = oidcsso.ErrNoRoles

// oidcBuiltinState lazily holds the built-in OIDC service of a Server.
type oidcBuiltinState struct {
	once sync.Once
	svc  *builtinOIDCService
	err  error
}

// getOIDCService returns the injected (enterprise) OIDC service if present,
// falling back to the built-in implementation.
func (a *Server) getOIDCService() (OIDCService, error) {
	if a.oidcAuthService != nil {
		return a.oidcAuthService, nil
	}
	st := &a.oidcBuiltin
	st.once.Do(func() {
		providers, err := oidcsso.NewProviderCache(a.closeCtx, a.clock)
		if err != nil {
			st.err = trace.Wrap(err)
			return
		}
		st.svc = &builtinOIDCService{
			a:         a,
			providers: providers,
			logger:    a.logger.With(teleport.ComponentKey, "oidc"),
		}
	})
	if st.err != nil {
		return nil, st.err
	}
	return st.svc, nil
}

// builtinOIDCService is the built-in implementation of [OIDCService].
type builtinOIDCService struct {
	a         *Server
	providers *oidcsso.ProviderCache
	logger    *slog.Logger
}

func (o *builtinOIDCService) getConnector(ctx context.Context, req types.OIDCAuthRequest) (types.OIDCConnector, error) {
	if req.SSOTestFlow {
		if req.ConnectorSpec == nil {
			return nil, trace.BadParameter("ConnectorSpec cannot be nil for SSOTestFlow")
		}
		if req.ConnectorID == "" {
			return nil, trace.BadParameter("ConnectorID cannot be empty")
		}
		// stateless test flow
		connector, err := types.NewOIDCConnector(req.ConnectorID, *req.ConnectorSpec)
		return connector, trace.Wrap(err)
	}

	connector, err := o.a.GetOIDCConnector(ctx, req.ConnectorID, true)
	return connector, trace.Wrap(err)
}

// CreateOIDCAuthRequest creates a new OIDC auth request and returns it with
// RedirectURL set to the IdP authorization endpoint.
func (o *builtinOIDCService) CreateOIDCAuthRequest(ctx context.Context, req types.OIDCAuthRequest) (*types.OIDCAuthRequest, error) {
	connector, err := o.getConnector(ctx, req)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// requests for a web session originate from the proxy, so they are trusted
	// and they're handled in such a way that minimizes misuse in the callback
	// endpoint; requests for a client session (as used by tsh login) need to be
	// checked, as they will point the browser away from the IdP or the web UI
	// after the authentication is done
	if !req.CreateWebSession {
		ceremonyType := sso.CeremonyTypeLogin
		if req.SSOTestFlow {
			ceremonyType = sso.CeremonyTypeTest
		}
		if err := sso.ValidateClientRedirect(req.ClientRedirectURL, ceremonyType, connector.GetClientRedirectSettings()); err != nil {
			return nil, trace.Wrap(err, InvalidClientRedirectErrorMessage)
		}
	}

	provider, err := o.providers.Get(ctx, connector.GetIssuerURL())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	redirectURL, err := services.GetRedirectURL(connector, req.ProxyAddress)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	req.StateToken, err = utils.CryptoRandomHex(defaults.TokenLenBytes)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	req.RedirectURL, req.PkceVerifier = oidcsso.AuthCodeURL(connector, provider, redirectURL, oidcsso.AuthCodeParams{
		State:        req.StateToken,
		LoginHint:    req.LoginHint,
		PKCEVerifier: req.PkceVerifier,
	})
	o.logger.DebugContext(ctx, "Creating OIDC auth request",
		"connector", connector.GetName(),
		"redirect_url", req.RedirectURL,
	)

	if err := o.a.Services.CreateOIDCAuthRequest(ctx, req, defaults.OIDCAuthRequestTTL); err != nil {
		return nil, trace.Wrap(err)
	}
	return &req, nil
}

// CreateOIDCAuthRequestForMFA is not supported by the built-in implementation.
func (o *builtinOIDCService) CreateOIDCAuthRequestForMFA(ctx context.Context, req types.OIDCAuthRequest) (*types.OIDCAuthRequest, error) {
	return nil, trace.NotImplemented("SSO MFA is not supported in this build")
}

// ValidateOIDCAuthCallback validates the OIDC callback, emits a login audit
// event and returns the resulting session and/or certificates.
func (o *builtinOIDCService) ValidateOIDCAuthCallback(ctx context.Context, q url.Values) (*authclient.OIDCAuthResponse, error) {
	diagCtx := NewSSODiagContext(types.KindOIDC, o.a)

	event := &apievents.UserLogin{
		Metadata: apievents.Metadata{
			Type: events.UserLoginEvent,
		},
		Method:             events.LoginMethodOIDC,
		ConnectionMetadata: authz.ConnectionMetadata(ctx),
	}

	auth, err := o.validateOIDCAuthCallback(ctx, diagCtx, q)
	diagCtx.Info.Error = trace.UserMessage(err)
	event.AppliedLoginRules = diagCtx.Info.AppliedLoginRules

	diagCtx.WriteToBackend(ctx)

	if claims := diagCtx.Info.OIDCClaims; claims != nil {
		attributes, encErr := apievents.EncodeMap(claims)
		if encErr != nil {
			event.Status.UserMessage = fmt.Sprintf("Failed to encode identity attributes: %v", encErr.Error())
			o.logger.DebugContext(ctx, "Failed to encode identity attributes", "error", encErr)
		} else {
			event.IdentityAttributes = attributes
		}
	}

	if err != nil {
		event.Code = events.UserSSOLoginFailureCode
		if diagCtx.Info.TestFlow {
			event.Code = events.UserSSOTestFlowLoginFailureCode
		}
		event.Status.Success = false
		event.Status.Error = trace.Unwrap(err).Error()
		event.Status.UserMessage = err.Error()

		if err := o.a.emitter.EmitAuditEvent(ctx, event); err != nil {
			o.logger.WarnContext(ctx, "Failed to emit OIDC login failed event", "error", err)
		}
		return nil, trace.Wrap(err)
	}

	event.Code = events.UserSSOLoginCode
	if diagCtx.Info.TestFlow {
		event.Code = events.UserSSOTestFlowLoginCode
	}
	event.Status.Success = true
	event.User = auth.Username

	if err := o.a.emitter.EmitAuditEvent(ctx, event); err != nil {
		o.logger.WarnContext(ctx, "Failed to emit OIDC login event", "error", err)
	}

	return auth, nil
}

func (o *builtinOIDCService) validateOIDCAuthCallback(ctx context.Context, diagCtx *SSODiagContext, q url.Values) (*authclient.OIDCAuthResponse, error) {
	if errParam := q.Get("error"); errParam != "" {
		// try to find request so the error gets logged against it.
		if state := q.Get("state"); state != "" {
			diagCtx.RequestID = state
			if req, err := o.a.Services.GetOIDCAuthRequest(ctx, state); err == nil {
				diagCtx.Info.TestFlow = req.SSOTestFlow
			}
		}
		errDesc := q.Get("error_description")
		oauthErr := trace.OAuth2("invalid_request", errParam, q)
		return nil, trace.WithUserMessage(oauthErr, "Identity provider returned error: %v [%v]", errDesc, errParam)
	}

	code := q.Get("code")
	if code == "" {
		oauthErr := trace.OAuth2("invalid_request", "code query param must be set", q)
		return nil, trace.WithUserMessage(oauthErr, "Invalid parameters received from identity provider.")
	}

	stateToken := q.Get("state")
	if stateToken == "" {
		oauthErr := trace.OAuth2("invalid_request", "missing state query param", q)
		return nil, trace.WithUserMessage(oauthErr, "Invalid parameters received from identity provider.")
	}
	diagCtx.RequestID = stateToken

	req, err := o.a.Services.GetOIDCAuthRequest(ctx, stateToken)
	if err != nil {
		return nil, trace.Wrap(err, "Failed to get OIDC Auth Request.")
	}
	diagCtx.Info.TestFlow = req.SSOTestFlow

	connector, err := o.getConnector(ctx, *req)
	if err != nil {
		return nil, trace.Wrap(err, "Failed to get OIDC connector.")
	}
	diagCtx.Info.OIDCClaimsToRoles = connector.GetClaimsToRoles()
	diagCtx.Info.OIDCConnectorTraitMapping = connector.GetTraitMappings()

	provider, err := o.providers.Get(ctx, connector.GetIssuerURL())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	redirectURL, err := services.GetRedirectURL(connector, req.ProxyAddress)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	result, err := oidcsso.ExchangeAndVerify(ctx, connector, provider, redirectURL, oidcsso.VerifyParams{
		Code:         code,
		PKCEVerifier: req.PkceVerifier,
		Nonce:        req.StateToken,
		Now:          o.a.GetClock().Now(),
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	claims := result.Claims
	diagCtx.Info.OIDCClaims = types.OIDCClaims(claims)
	identity := oidcsso.IdentityFromClaims(result.IDToken, claims)
	diagCtx.Info.OIDCIdentity = &identity

	if err := oidcsso.CheckEmailVerified(connector, claims); err != nil {
		return nil, trace.Wrap(err)
	}

	username, err := oidcsso.UsernameFromClaims(connector, claims)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	params, err := o.calculateOIDCUser(ctx, diagCtx, connector, claims, username, result.IDToken.Subject, req)
	if err != nil {
		return nil, trace.Wrap(err, "Failed to calculate user attributes.")
	}

	diagCtx.Info.CreateUserParams = &types.CreateUserParams{
		ConnectorName: params.ConnectorName,
		Username:      params.Username,
		KubeGroups:    params.KubeGroups,
		KubeUsers:     params.KubeUsers,
		Roles:         params.Roles,
		Traits:        params.Traits,
		SessionTTL:    types.Duration(params.SessionTTL),
	}

	user, err := o.createOIDCUser(ctx, params, req.SSOTestFlow)
	if err != nil {
		return nil, trace.Wrap(err, "Failed to create user from provided parameters.")
	}

	if err := o.a.CallLoginHooks(ctx, user); err != nil {
		return nil, trace.Wrap(err)
	}

	userState, err := o.a.GetUserOrLoginState(ctx, user.GetName())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	externalIdentity := types.ExternalIdentity{
		ConnectorID: params.ConnectorName,
		Username:    params.Username,
		UserID:      params.UserID,
	}

	// In test flow skip signing and creating web sessions.
	if req.SSOTestFlow {
		diagCtx.Info.Success = true
		return &authclient.OIDCAuthResponse{
			Req:      OIDCAuthRequestFromProto(req),
			Identity: externalIdentity,
			Username: params.Username,
		}, nil
	}

	return o.makeOIDCAuthResponse(ctx, req, userState, externalIdentity, params.SessionTTL)
}

func (o *builtinOIDCService) calculateOIDCUser(ctx context.Context, diagCtx *SSODiagContext, connector types.OIDCConnector, claims map[string]any, username, subject string, request *types.OIDCAuthRequest) (*CreateUserParams, error) {
	p := CreateUserParams{
		ConnectorName: connector.GetName(),
		Username:      username,
		UserID:        subject,
	}

	traits := oidcsso.ClaimsToTraits(claims)
	diagCtx.Info.OIDCTraitsFromClaims = traits

	roles, warnings, err := oidcsso.MapRoles(connector, traits)
	if len(warnings) > 0 {
		diagCtx.Info.OIDCClaimsToRolesWarnings = &types.SSOWarnings{
			Message:  "Failed to map claims to roles",
			Warnings: warnings,
		}
	}
	if err != nil {
		return nil, trace.Wrap(err)
	}
	p.Roles = roles

	// Default the logins trait to the username, so that role templates such
	// as {{internal.logins}} resolve to something useful out of the box.
	if _, ok := traits[constants.TraitLogins]; !ok {
		traits[constants.TraitLogins] = []string{username}
	}

	evaluationOutput, err := o.a.GetLoginRuleEvaluator().Evaluate(ctx, &loginrule.EvaluationInput{
		Traits: traits,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	p.Traits = evaluationOutput.Traits
	diagCtx.Info.AppliedLoginRules = evaluationOutput.AppliedRules

	p.KubeGroups = p.Traits[constants.TraitKubeGroups]
	p.KubeUsers = p.Traits[constants.TraitKubeUsers]

	// Pick smaller for role: session TTL from role or requested TTL.
	fetchedRoles, err := services.FetchRolesWithContext(p.Roles, o.a, services.RoleTemplateContext{
		Username: p.Username,
		Traits:   p.Traits,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	roleTTL := fetchedRoles.AdjustSessionTTL(apidefaults.MaxCertDuration)
	p.SessionTTL = utils.MinTTL(roleTTL, request.CertTTL)

	return &p, nil
}

func (o *builtinOIDCService) createOIDCUser(ctx context.Context, p *CreateUserParams, dryRun bool) (types.User, error) {
	o.logger.DebugContext(ctx, "Generating dynamic OIDC identity",
		"connector_name", p.ConnectorName,
		"user_name", p.Username,
		"role", p.Roles,
		"dry_run", dryRun,
	)

	now := o.a.GetClock().Now().UTC()
	expires := now.Add(p.SessionTTL)

	user := &types.UserV2{
		Kind:    types.KindUser,
		Version: types.V2,
		Metadata: types.Metadata{
			Name:      p.Username,
			Namespace: apidefaults.Namespace,
			Expires:   &expires,
		},
		Spec: types.UserSpecV2{
			Roles:  p.Roles,
			Traits: p.Traits,
			OIDCIdentities: []types.ExternalIdentity{{
				ConnectorID: p.ConnectorName,
				Username:    p.Username,
				UserID:      p.UserID,
			}},
			CreatedBy: types.CreatedBy{
				User: types.UserRef{Name: teleport.UserSystem},
				Time: now,
				Connector: &types.ConnectorRef{
					Type:     constants.OIDC,
					ID:       p.ConnectorName,
					Identity: p.Username,
				},
			},
		},
	}

	if dryRun {
		return user, nil
	}

	existingUser, err := o.a.Services.GetUser(ctx, p.Username, false)
	if err != nil && !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}

	if existingUser != nil {
		ref := user.GetCreatedBy().Connector
		if !ref.IsSameProvider(existingUser.GetCreatedBy().Connector) {
			return nil, trace.AlreadyExists("local user %q already exists and is not an OIDC user",
				existingUser.GetName())
		}

		user.SetRevision(existingUser.GetRevision())
		if _, err := o.a.UpdateUser(ctx, user); err != nil {
			return nil, trace.Wrap(err)
		}
	} else {
		if _, err := o.a.CreateUser(ctx, user); err != nil {
			return nil, trace.Wrap(err)
		}
	}

	return user, nil
}

func (o *builtinOIDCService) makeOIDCAuthResponse(
	ctx context.Context,
	req *types.OIDCAuthRequest,
	userState services.UserState,
	identity types.ExternalIdentity,
	sessionTTL time.Duration,
) (*authclient.OIDCAuthResponse, error) {
	a := o.a
	auth := authclient.OIDCAuthResponse{
		Req:      OIDCAuthRequestFromProto(req),
		Identity: identity,
		Username: userState.GetName(),
	}

	// If the request is coming from a browser, create a web session.
	if req.CreateWebSession {
		session, err := a.CreateWebSessionFromReq(ctx, NewWebSessionRequest{
			User:                 userState.GetName(),
			Roles:                userState.GetRoles(),
			Traits:               userState.GetTraits(),
			SessionTTL:           sessionTTL,
			LoginTime:            a.clock.Now().UTC(),
			LoginIP:              req.ClientLoginIP,
			LoginUserAgent:       req.ClientUserAgent,
			AttestWebSession:     true,
			CreateDeviceWebToken: true,
			Scope:                req.Scope,
		})
		if err != nil {
			return nil, trace.Wrap(err, "Failed to create web session.")
		}

		auth.Session = session
	}

	// If a public key was provided, sign it and return a certificate.
	if len(req.SshPublicKey) != 0 || len(req.TlsPublicKey) != 0 {
		sshCert, tlsCert, err := a.CreateSessionCerts(ctx, &SessionCertsRequest{
			UserState:               userState,
			SessionTTL:              sessionTTL,
			SSHPubKey:               req.SshPublicKey,
			TLSPubKey:               req.TlsPublicKey,
			SSHAttestationStatement: hardwarekey.AttestationStatementFromProto(req.SshAttestationStatement),
			TLSAttestationStatement: hardwarekey.AttestationStatementFromProto(req.TlsAttestationStatement),
			Compatibility:           req.Compatibility,
			RouteToCluster:          req.RouteToCluster,
			KubernetesCluster:       req.KubernetesCluster,
			LoginIP:                 req.ClientLoginIP,
			Scope:                   req.Scope,
		})
		if err != nil {
			return nil, trace.Wrap(err, "Failed to create session certificate.")
		}

		clusterName, err := a.GetClusterName(ctx)
		if err != nil {
			return nil, trace.Wrap(err, "Failed to obtain cluster name.")
		}

		auth.Cert = sshCert
		auth.TLSCert = tlsCert

		// Return the host CA for this cluster only.
		authority, err := a.GetCertAuthority(ctx, types.CertAuthID{
			Type:       types.HostCA,
			DomainName: clusterName.GetClusterName(),
		}, false)
		if err != nil {
			return nil, trace.Wrap(err, "Failed to obtain cluster's host CA.")
		}
		auth.HostSigners = append(auth.HostSigners, authority)
	}

	if opts, err := a.ClientOptionsForLogin(userState); err == nil {
		auth.ClientOptions = opts
	} else {
		o.logger.WarnContext(ctx, "Failed to calculate client options for OIDC login", "username", userState.GetName(), "error", err)
	}

	return &auth, nil
}

// OIDCAuthRequestFromProto converts the types.OIDCAuthRequest to authclient.OIDCAuthRequest.
func OIDCAuthRequestFromProto(req *types.OIDCAuthRequest) authclient.OIDCAuthRequest {
	return authclient.OIDCAuthRequest{
		ConnectorID:       req.ConnectorID,
		CSRFToken:         req.CSRFToken,
		SSHPubKey:         req.SshPublicKey,
		TLSPubKey:         req.TlsPublicKey,
		CreateWebSession:  req.CreateWebSession,
		ClientRedirectURL: req.ClientRedirectURL,
	}
}

/*
validateOIDCAuthCallback validates OIDC auth callback redirect. The proxy
calls it through authclient.HTTPClient.ValidateOIDCAuthCallback.

	POST /:version/oidc/requests/validate

	Success response: authclient.OIDCAuthRawResponse
*/
func (s *APIServer) validateOIDCAuthCallback(auth *ServerWithRoles, w http.ResponseWriter, r *http.Request, p httprouter.Params, version string) (any, error) {
	var req authclient.ValidateOIDCAuthCallbackReq
	if err := httplib.ReadJSON(r, &req); err != nil {
		return nil, trace.Wrap(err)
	}
	response, err := auth.ValidateOIDCAuthCallback(r.Context(), req.Query)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	raw := authclient.OIDCAuthRawResponse{
		Username:      response.Username,
		Identity:      response.Identity,
		Cert:          response.Cert,
		TLSCert:       response.TLSCert,
		Req:           response.Req,
		MFAToken:      response.MFAToken,
		ClientOptions: response.ClientOptions,
	}
	if response.Session != nil {
		rawSession, err := services.MarshalWebSession(
			response.Session, services.WithVersion(version), services.PreserveRevision())
		if err != nil {
			return nil, trace.Wrap(err)
		}
		raw.Session = rawSession
	}
	raw.HostSigners = make([]json.RawMessage, len(response.HostSigners))
	for i, ca := range response.HostSigners {
		data, err := services.MarshalCertAuthority(
			ca, services.WithVersion(version), services.PreserveRevision())
		if err != nil {
			return nil, trace.Wrap(err)
		}
		raw.HostSigners[i] = data
	}
	return &raw, nil
}
