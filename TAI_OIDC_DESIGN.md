# Teleport v18.10.0 集成 TAI OIDC 登录 — 设计文档

> 分支：`feature/tai-oidc-login`（基于 tag `v18.10.0`，commit `ddaa46b8`）
> 状态：已实现（P1–P3）；实现与设计的差异见 §9
> 许可合规：本仓库为 AGPLv3 开源仓库，修改在开源协议允许范围内进行，保留原有版权声明

---

## 1. 背景与目标

### 1.1 需求

在 Teleport v18.10.0（OSS 开源版）中接入 OIDC 登录，OIDC Provider 为内部 **TAI 统一认证**。

### 1.2 TAI 认证特性（来自 `gin-tai-login` 样例项目）

| 特性 | 说明 |
|---|---|
| 协议 | 标准 OIDC 授权码流程（Authorization Code Flow） |
| Discovery | 标准 `.well-known/openid-configuration` |
| **特殊点** | ID token 中用户名 claim 为 **`user_name`**（非标准 `preferred_username` / `email`） |
| scopes | `openid`, `profile`（样例未申请 `email` scope） |
| PKCE | 样例未使用（Teleport connector 的 `pkce_mode` 默认关闭，兼容） |
| 参考实现 | `coreos/go-oidc/v3` + `golang.org/x/oauth2` 验证通过 |

TAI 返回的典型 claims：

```json
{
  "sub": "user123",
  "user_name": "zhangsan",
  "name": "张三",
  "iat": 1707734400,
  "exp": 1707738000
}
```

### 1.3 目标

1. Web UI（浏览器）通过 TAI 完成 SSO 登录，签发 web session
2. CLI（`tsh login --auth=oidc`）通过 TAI 完成 SSO 登录，签发 SSH/TLS 证书
3. `tctl sso test oidc` 可对 TAI connector 做全链路诊断
4. TAI 的 `user_name` claim 映射为 Teleport 用户名
5. 保留 enterprise `OIDCService` 注入点，不破坏后续上游合并

### 1.4 非目标（本期不做）

- SSO + MFA 流程（`CreateOIDCAuthRequestForMFA` 返回 `NotImplemented`）
- Request Object / EntraID groups / Google service account 等 enterprise 扩展
- 向上游开源仓库回馈此变更

---

## 2. 现状调研（v18.10.0 架构事实）

### 2.1 核心事实：OIDC 协议实现不在 OSS 仓库

Teleport v18 中 OIDC 是 Enterprise-only 功能。OSS 仓库只有类型定义、委托接口、客户端流程和工具；真正的协议实现（provider discovery、ID token 验证、claims 映射、web 路由）在私有仓库 `github.com/gravitational/teleport/e`（本地不可用）。

证据（行号均为 v18.10.0）：

| 位置 | 事实 |
|---|---|
| `lib/auth/oidc.go:34-40` | `OIDCService` 接口定义 + `errOIDCNotImplemented`（"OIDC is only available in enterprise subscriptions"） |
| `lib/auth/oidc.go:128-159` | `CreateOIDCAuthRequest` / `CreateOIDCAuthRequestForMFA` / `ValidateOIDCAuthCallback` 委托给 `a.oidcAuthService`，为 nil 时报 enterprise-only 错误 |
| `lib/auth/auth.go:1336,1616` | `Server.oidcAuthService` 字段 + `SetOIDCService(svc)` enterprise 注入点 |
| `lib/auth/auth_with_roles.go:4434,4455,4475,4537` | **4 处** `entitlements.OIDC` 门控（connector Upsert/Update/Create + CreateOIDCAuthRequest） |
| `lib/modules/modules.go:417-433` | OSS `defaultModules.Features()` 的 entitlements 中无 OIDC（即 disabled） |
| `lib/web/apiserver.go:980-982` | 只注册了 GitHub SSO 路由，无 OIDC web 路由 |

关键点：4 处门控全部读取同一个 `modules.GetModules().Features().GetEntitlement(entitlements.OIDC).Enabled`，因此**只需在 `modules.go` 启用该 entitlement，4 处全部放行**。

### 2.2 OSS 已备齐的"积木"

| 积木 | 位置（v18.10.0） | 状态 |
|---|---|---|
| `OIDCConnectorV3` 类型（含 `username_claim`、`claims_to_roles`、`GetTraitMappings()`） | `api/types/oidc.go:370-400` | ✅ 完整 |
| Connector 校验 `CheckAndSetDefaults` | `api/types/oidc.go:408-505` | ✅ 完整 |
| Connector CRUD + 审计事件 + `tctl create` | `lib/auth/oidc.go:43-126` | ✅ 完整 |
| `OIDCAuthRequest` 类型 + 后端存储 | `api/proto/.../types.proto:5659`、`lib/services/local/users.go` | ✅ 完整 |
| gRPC server + authclient + `OIDCAuthResponse` | `lib/auth/grpcserver.go`、`lib/auth/authclient/clt.go:849-872` | ✅ 完整 |
| 前端 SSO 按钮自动生成（`/webconfig` 按 connector 生成，URL 模板 `/v1/webapi/oidc/login/web?...`） | `lib/web/apiserver.go:1895-1910`、`api/client/webclient/webconfig.go:30` | ✅ 完整 |
| `tsh login` 客户端流程（POST `/webapi/oidc/login/console` + localhost callback） | `lib/client/sso.go:74-117`、`lib/client/sso/redirector.go` | ✅ 完整 |
| `tctl sso test oidc`（SSOTestFlow 诊断，`SSODiagnosticInfo` 含 OIDC 字段） | `tool/tctl/sso/tester/oidc.go` | ✅ 完整 |
| 依赖库 `coreos/go-oidc/v3`、`golang.org/x/oauth2`、go-jose | `go.mod`（已有，indirect → 转 direct） | ✅ 已有 |
| redirect_url 选择逻辑 | `lib/services/oidc.go:33-74`（`GetRedirectURL`） | ✅ 完整 |
| **完整 SSO 参照实现（GitHub）** | `lib/auth/github.go` + `lib/web/apiserver.go:2360-2519` | ✅ 最佳模板 |
| claims→roles glob 匹配参照 | `lib/web/apiserver.go:2093`（`globMatch`） | ✅ 可参照 |

**结论：需要补齐的只有 4 块** —— ① 协议核心实现 ② 默认接线 ③ entitlement 放行 ④ web 路由。TAI 的 `user_name` claim 通过现成的 `username_claim` 配置项即可支持，无需任何硬编码。

### 2.3 登录流程全景（实现后）

```
=== Web UI 登录 ===
浏览器                    Proxy Web Handler(新增)         Auth Server(新增实现)           TAI IdP
  │ GET /v1/webapi/oidc/login/web?connector_id=tai&redirect_url=...      │                              │
  │──────────────────────────►│  gRPC CreateOIDCAuthRequest             │                              │
  │                           │────────────────────────────────────────►│ OIDC discovery(.well-known)  │
  │                           │                                         │─────────────────────────────►│
  │                           │◄── OIDCAuthRequest(StateToken, RedirectURL=TAI授权URL) ──┤              │
  │◄── 302 → TAI 授权页 ──────┤                                         │                              │
  │──────────────────────────────────────────────────────────────────────────────────────────────────►│ 用户认证
  │ GET /v1/webapi/oidc/callback?code&state                              │                              │
  │──────────────────────────►│  gRPC ValidateOIDCAuthCallback(q)        │                              │
  │                           │────────────────────────────────────────►│ code↔token + ID token 验证    │
  │                           │                                         │ (签名/iss/aud/exp 由 go-oidc) │
  │                           │                                         │ claims[user_name]→username    │
  │                           │                                         │ claims_to_roles→traits→roles  │
  │                           │                                         │ upsert user + web session/证书 │
  │◄── 302 回 Web UI（+session cookie）──┤                               │                              │

=== CLI 登录 (tsh login --auth=oidc) ===
tsh ── GET /webapi/ping ──► Proxy（返回 Auth.OIDC{Name,Display,IssuerURL}）
tsh ── 起本地 localhost:<port>/callback HTTP 服务（lib/client/sso/redirector.go，已有）
tsh ── POST {proxy}/webapi/oidc/login/console（公钥/回调URL/PKCE，SSOLoginConsoleReq）
       └─► Proxy → Auth.CreateOIDCAuthRequest → 返回 TAI RedirectURL
tsh ── 打开浏览器 → TAI 认证
       └─► TAI → Proxy /v1/webapi/oidc/callback → ValidateOIDCAuthCallback
            → 签发证书 → 302 到 localhost/callback（secret.Key 加密的 CLILoginResponse）
tsh ── WaitForResponse() 解密获得证书 → 保存 profile
```

---

## 3. 总体设计

```
┌────────┐                                        ┌──────────────────┐
│ 浏览器/ │  /v1/webapi/oidc/login/web            │  L4: Proxy Web    │  lib/web/apiserver.go（修改）
│  tsh   │ ────────────────────────────────────► │  Handler（新增）   │  3 条路由 + 3 个 handler
└───┬────┘        /v1/webapi/oidc/callback       └────────┬─────────┘
    │             /webapi/oidc/login/console               │ gRPC（已有）
    │                                                      ▼
    │             ┌──────────────────────────────────────────────────┐
    │             │  L1: Auth Server OIDC 核心实现（新增）              │ lib/auth/oidc_auth.go
    │             │  discovery 缓存 / code↔token / ID token 验证       │
    │             │  claims[user_name]→username / claims→traits→roles  │
    │             │  upsert user / web session / 证书签发               │
    │             └──────────────────────────────────────────────────┘
    │                             ▲
    │             ┌───────────────┴──────────────┐
    │             │ L2: 默认接线（修改）            │ lib/auth/oidc.go
    │             │ oidcAuthService==nil 时走内建  │
    │             └──────────────────────────────┘
    │             ┌──────────────────────────────┐
    │             │ L3: entitlement 放行（修改）    │ lib/modules/modules.go
    │             │ OIDC: {Enabled: true}         │  → 4 处门控全部放行
    │             └──────────────────────────────┘
    │             ┌──────────────────────────────┐
    │             │ L5: TAI 适配（配置为主）        │ connector YAML + 可选
    │             │ username_claim: user_name     │ tctl preset/图标/文档
    │             └──────────────────────────────┘
```

| 层 | 内容 | 文件 | 类型 |
|---|---|---|---|
| **L1** | OIDC 协议核心实现 | 新增 `lib/auth/oidc_auth.go`（~700 行） | 新文件 |
| **L2** | 默认接线（保留 enterprise 注入点） | 修改 `lib/auth/oidc.go:128-159` | 小改 |
| **L3** | OIDC entitlement 放行 | 修改 `lib/modules/modules.go:425-431` | 1 行 |
| **L4** | Web 三条路由 + handler | 修改 `lib/web/apiserver.go` | 新增 ~150 行 |
| **L5** | TAI 适配 | connector 配置 + 可选 `tool/tctl/sso/configure/oidc.go` preset、前端图标、文档 | 配置/可选 |
| **L6** | 测试 | 新增 `lib/auth/oidc_auth_test.go`、`lib/web/apiserver_test.go` 追加 | 新文件 |

---

## 4. 详细设计

### 4.1 L1：核心协议实现（新文件 `lib/auth/oidc_auth.go`）

依赖：`github.com/coreos/go-oidc/v3`、`golang.org/x/oauth2`（与 TAI 样例同一套库，行为一致；`go.mod` 中由 indirect 转 direct）。

#### 4.1.1 结构体与 provider 缓存

```go
// oidcAuth implements OIDCService for OSS builds.
type oidcAuth struct {
    a *Server
    // providers caches *oidc.Provider by issuer URL (TTL ~10min),
    // avoiding a discovery round-trip on every login.
    providers *utils.FnCache
}

func (a *Server) oidcAuth() *oidcAuth  // lazy init, 单例挂于 Server
```

#### 4.1.2 `CreateOIDCAuthRequest`（参照 `lib/auth/github.go:142-179`）

1. **取 connector**：`req.SSOTestFlow` 为 true 时用 `req.ConnectorSpec` 构造的临时 connector（`tctl sso test` 用，不落库），否则 `a.GetOIDCConnector(ctx, req.ConnectorID, true)`（带 secrets）
2. **回调白名单校验**（非 web session 请求时，即 tsh/console 流程）：
   `sso.ValidateClientRedirect(req.ClientRedirectURL, ceremonyType, conn.GetClientRedirectSettings())`
3. **生成 state**：`req.StateToken = utils.CryptoRandomHex(defaults.TokenLenBytes)`
4. **discovery**：`providers.Get(issuerURL)` → `oidc.NewProvider(ctx, issuerURL)`（TAI 标准 discovery 直接兼容）
5. **构造授权 URL**（`oauth2.Config.AuthCodeURL` + 附加参数）：
   - `redirect_uri`：`services.GetRedirectURL(conn, req.ProxyAddress)`（现成，`lib/services/oidc.go:33`）
   - `scope`：`conn.GetScope()`；为空时默认 `["openid", "email"]`（TAI connector 显式配 `["openid", "profile"]`）
   - 附加 `prompt`（默认 `select_account`，`api/types/oidc.go:165`）、`acr_values`、`max_age`、`login_hint`（web 层透传）
   - `conn.IsPKCEEnabled()` 时生成并暂存 code verifier/challenge
6. **持久化**：`req.SetExpiry(now + 10min)` → `a.Services.CreateOIDCAuthRequest(ctx, req)`（现成）
7. 返回 `req`（其 `RedirectURL` 字段即 IdP 授权 URL）

#### 4.1.3 `ValidateOIDCAuthCallback`（参照 `lib/auth/github.go:433-740` 骨架）

1. **取回请求**：`state := q.Get("state")` → `a.GetOIDCAuthRequest(ctx, state)`；不存在/过期 → 错误。检查 `error`/`error_description` 参数（IdP 侧失败）
2. **换 token**：`oauth2.Config{ClientID, ClientSecret, Endpoint, RedirectURL}.Exchange(ctx, code)`
3. **验证 ID token（安全关键）**：
   ```go
   rawIDToken, ok := token.Extra("id_token").(string)  // 无 id_token → 报错（要求 scope 含 openid）
   verifier := provider.Verifier(&oidc.Config{ClientID: conn.GetClientID()})
   idToken, err := verifier.Verify(ctx, rawIDToken)
   ```
   由 go-oidc 强制完成：**签名（JWKS 拉取与轮换）、`iss` 精确匹配、`aud == client_id`、`exp`/`iat` 校验**。不自行解析未验签的 payload。
4. **解析 claims**：`idToken.Claims(&claims)`（`map[string]interface{}`）→ 填入 `SSODiagContext`（`OIDCClaims`、`OIDCConnectorTraitMapping` 等，`tctl sso test` 诊断直接可用）
5. **claims → username（TAI 对接核心）**：
   ```go
   usernameClaim := conn.GetUsernameClaim()
   if usernameClaim == "" { usernameClaim = "email" }  // Teleport 默认
   username := claims[usernameClaim].(string)           // TAI: username_claim: user_name
   ```
   - 校验：非空、去空白、`strings.ToLower`（Teleport 用户名约定小写）、长度 ≤ `defaults.MaxUserNameLength`
   - 当 claim 为 `email` 时校验 `email_verified`（除非 `conn.GetAllowUnverifiedEmail()`）；TAI 用 `user_name` 规避此问题
   - `sub` 一并提取存入 identity
6. **claims → traits → roles（新增辅助函数）**：
   ```go
   // traitsAndRolesFromClaims computes user traits and mapped roles from ID token claims.
   // 语义与 Teleport enterprise/SAML 的 claims_to_roles 对齐：
   // - traits[claim] = claim 的全部值（string 或 []any 归一化为 []string）
   //   traits 写入用户对象，供角色模板 {{external.user_name}} 等引用
   // - 对每条 mapping：claim 值与 mapping.Value 做 glob 匹配（'*' 通配，
   //   参照 lib/web/apiserver.go:2093 globMatch），匹配则并入 roles
   // - 无任何角色命中 → 报错（与 GitHub "no teams" 行为对齐）
   func traitsAndRolesFromClaims(claims map[string]any, mappings types.TraitMappingSet) (map[string][]string, []string, error)
   ```
   输入 `mappings = conn.GetTraitMappings()`（现成，`api/types/oidc.go:390`）。
7. **用户落库**（参照 `calculateGithubUser` / `createGithubUser`）：
   - 计算 session TTL（用户/角色默认值下限）
   - upsert `types.UserV2`：`Traits`（第 6 步）、`Roles`（第 6 步）、由 connector 名派生的 logins（`{{external.logins}}` 模板所需）
   - `SSOTestFlow` 时跳过落库（dry-run）
   - `a.CallLoginHooks(ctx, user)` → `a.GetUserOrLoginState(ctx, user.GetName())`
8. **产出响应**（结构复用 `makeGithubAuthResponse` 的模式，`lib/auth/github.go:662-740`）：
   - `req.CreateWebSession` → `a.CreateWebSessionFromReq(NewWebSessionRequest{...})`（web 登录）
   - `req.SshPublicKey` / `req.TlsPublicKey` 非空 → `a.CreateSessionCerts(...)` + `a.GetCertAuthority(HostCA)` → `HostSigners`（tsh 登录）
   - `a.ClientOptionsForLogin(userState)` → `ClientOptions`
   - 返回 `authclient.OIDCAuthResponse{Username, Identity, Session, Cert, TLSCert, Req, HostSigners, ClientOptions}`（类型现成，`lib/auth/authclient/clt.go:849-872`）
   - 发出 `events.UserLogin` 审计事件（`LoginMethodOIDC`）

#### 4.1.4 `CreateOIDCAuthRequestForMFA`

v1 返回 `trace.NotImplemented("SSO MFA is not supported in this build")`。接口保留，后续版本可补。

### 4.2 L2：默认接线（修改 `lib/auth/oidc.go:128-159`）

三个委托方法改为：**enterprise 注入优先，否则走 OSS 内建实现**：

```go
func (a *Server) CreateOIDCAuthRequest(ctx context.Context, req types.OIDCAuthRequest) (*types.OIDCAuthRequest, error) {
    if a.oidcAuthService != nil {  // enterprise 注入（保留注入点）
        return a.oidcAuthService.CreateOIDCAuthRequest(ctx, req)
    }
    return a.oidcAuth().CreateOIDCAuthRequest(ctx, req)  // OSS 内建（新增）
}
```

`ValidateOIDCAuthCallback`、`CreateOIDCAuthRequestForMFA` 同改；删除 `errOIDCNotImplemented`。

### 4.3 L3：entitlement 放行（修改 `lib/modules/modules.go:425-431`）

```go
func (p *defaultModules) Features() Features {
    ...
    Entitlements: map[entitlements.EntitlementKind]EntitlementInfo{
        entitlements.App:     {Enabled: true, Limit: 0},
        entitlements.DB:      {Enabled: true, Limit: 0},
        ...
        entitlements.OIDC:    {Enabled: true, Limit: 0},  // ← 新增：本 fork 启用 OIDC
    },
}
```

一处修改，`auth_with_roles.go` 的 4 处门控（4434/4455/4475/4537）全部放行；`setLegacyLogic`（`modules.go:150`）与 `entitlements.BackfillFeatures` 自动联动，无需其他改动。

### 4.4 L4：Web 路由（修改 `lib/web/apiserver.go`）

路由路径由三处既有代码**固定决定**（不可自定）：

| 路由 | 决定来源 |
|---|---|
| `GET /v1/webapi/oidc/login/web` | 前端按钮 URL 模板 `webconfig.go:30`（`WebConfigAuthProviderOIDCURL`） |
| `GET /v1/webapi/oidc/callback` | `tctl sso configure oidc` 默认 redirect_url（`tool/tctl/sso/configure/oidc.go`） |
| `POST /webapi/oidc/login/console` | `tsh` 客户端 `lib/client/sso.go:106` |

注册（与 GitHub 路由同段，`apiserver.go:980` 旁）：

```go
// OIDC connector handlers
h.GET("/v1/webapi/oidc/login/web", h.WithRedirect(h.oidcLoginWeb))
h.GET("/v1/webapi/oidc/callback", h.WithMetaRedirect(h.oidcCallback))
h.POST("/webapi/oidc/login/console", h.WithLimiter(h.oidcLoginConsole))
```

三个 handler 与 GitHub 版（`apiserver.go:2360-2519`）逐行同构：

- **`oidcLoginWeb`**：`ParseSSORequestParams(r)` → `CreateOIDCAuthRequest{CreateWebSession: true, CSRFToken, ConnectorID, ClientRedirectURL, ClientLoginIP, ClientUserAgent, ProxyAddress}` → 返回 `response.RedirectURL`
- **`oidcLoginConsole`**：读 `client.SSOLoginConsoleReq`（`CheckAndSetDefaults` 含 PKCE verifier）→ `CreateOIDCAuthRequest{SshPublicKey, TlsPublicKey, CertTTL, ...}` → 返回 `SSOLoginConsoleResponse{RedirectURL}`；`InvalidClientRedirectErrorMessage` → `SSOLoginFailureInvalidRedirect`
- **`oidcCallback`**：`ValidateOIDCAuthCallback(ctx, r.URL.Query())` →
  - 失败：借 `state` 查 `GetOIDCAuthRequest` 取 `ClientRedirectURL` 回传错误（`RedirectURLWithError`），否则 `LoginFailedBadCallbackRedirectURL`
  - 成功 + `CreateWebSession`：`SSOSetWebSessionAndRedirectURL`（设 session cookie）→ 处理 DeviceWebToken → 302 回 Web UI
  - 成功 + console：`ConstructSSHResponse`（加密证书给 localhost callback）

**前端零改动**：`getWebConfig`（`apiserver.go:1895-1910`）已为每个 OIDC connector 自动生成登录按钮（type/URL/display），TAI connector 创建后按钮自动出现。

### 4.5 L5：TAI 适配

#### 4.5.1 连接器配置（核心对接点，纯配置）

```yaml
kind: oidc
version: v3
metadata:
  name: tai
spec:
  # TAI OIDC 端点（需与 TAI 后台登记的回调地址一致）
  issuer_url: https://<TAI_ISSUER_HOST>
  client_id: <TAI_CLIENT_ID>
  client_secret: <TAI_CLIENT_SECRET>
  redirect_url:
  - https://<proxy公网地址>/v1/webapi/oidc/callback
  # TAI 支持的 scopes（样例为 openid,profile）
  scope: [openid, profile]
  # ★ TAI 特有：用户名 claim 为 user_name（替代默认 email）
  username_claim: user_name
  display: "TAI 统一认证"
  claims_to_roles:
  - claim: user_name
    value: "*"
    roles: [access]   # 起步全员 access；可按 user_name/其他 claim 细分
```

配置要点：
- `username_claim: user_name` —— 唯一的 TAI 特有配置，无需改代码
- 若 TAI 后续支持 `email` scope，可切换为默认 email 模式
- 多角色映射示例：`claim: user_name, value: "admin-*", roles: [editor, auditor]`

#### 4.5.2 可选增强（提升易用性，非必需）

| 项 | 位置 | 内容 |
|---|---|---|
| provider 常量 | `constants.go:569-581` | 加 `TAI = "tai"` |
| tctl preset | `tool/tctl/sso/configure/oidc.go:71-130`（`oidcPresets`） | 加 `tai` 预设（预填 `username_claim: user_name`、`scope: [openid,profile]`、`display: TAI`）；`--provider` 帮助文本同步 |
| 前端图标 | `web/packages/teleport/src/AuthConnectors/ssoIcons/getSsoIcon.tsx` | 加 TAI 关键词匹配（fallback 为通用 OIDC 图标，功能不受影响） |
| 用户文档 | `docs/pages/zero-trust-access/sso/integrate-idp/tai.mdx`（新增） | TAI 接入指南；`integrate-idp.mdx:129-145` provider 列表同步 |

### 4.6 L6：测试

| 文件 | 覆盖内容 |
|---|---|
| 新增 `lib/auth/oidc_auth_test.go` | `httptest` 搭假 TAI IdP（手写 `/.well-known/openid-configuration`、`/token`、JWKS；go-jose 生成 RSA 密钥签发 JWT）：① `user_name` claim → username；② `claims_to_roles` glob 映射（精确/通配/多值 claim）；③ traits 归一化（string 与 array claim）；④ web session 与证书签发（`CreateWebSession` / `SshPublicKey` 两分支）；⑤ 负例：state 过期、aud 不符、签名错误、username claim 缺失、无角色命中、`email_verified=false` |
| `lib/web/apiserver_test.go` 追加 | 三条 OIDC 路由的 handler 测试（参照既有 GitHub SSO 测试模式，mock `OIDCService`——参照 `lib/auth/sso_mfa_test.go:732-762` 的 `fakeSSOService`） |
| `lib/modules` 相关 | entitlement 启用后 `Features()` 断言（快照测试自动覆盖） |

---

## 5. 安全设计要点

1. **ID token 完整校验**：签名（JWKS 自动拉取与轮换）、`iss` 精确匹配、`aud == client_id`、`exp/iat` 均由 go-oidc Verifier 强制；不解析未验签 payload；不信任 userinfo 端点（仅用 ID token claims）
2. **防 CSRF/重放**：`state` 为 crypto-random、一次性、10 分钟 TTL、用后即删；PKCE 可按 connector 开启（`pkce_mode: enabled`）
3. **回调白名单**：`sso.ValidateClientRedirect` + connector `client_redirect_settings` 的 CIDR/HTTPS 校验（现成），防开放重定向
4. **秘密保护**：`client_secret` 不写日志/审计事件/SSO 诊断信息（诊断仅记录 claims 子集与映射结果）
5. **email 验证**：使用 `email` claim 时强制 `email_verified`（`allow_unverified_email` 默认 false）；TAI 走 `user_name` 不受影响
6. **权限边界不变**：`CreateWebSession` 仅 builtin `RoleProxy` 可用（`auth_with_roles.go:4560` 现成检查保留）；connector CRUD 保留既有 RBAC + MFA 检查
7. **username 规范化**：小写化 + 长度上限，防注入与资源滥用

---

## 6. 实施计划

| 阶段 | 内容 | 交付验证 | 预估 |
|---|---|---|---|
| **P1** | L1+L2+L3（核心实现 + 接线 + entitlement） | `tctl sso test oidc --connector=tai` 对真实 TAI 全链路通过（SSOTestFlow 诊断输出 claims/roles/identity） | 2-3 天 |
| **P2** | L4（web 路由） | Web UI 按钮登录成功；`tsh login --auth=oidc`（或 `--auth=tai`）签发证书成功 | 1-2 天 |
| **P3** | L5+L6（preset/文档/图标 + 测试） | 单测全绿（`go test ./lib/auth/... ./lib/web/... ./lib/modules/...`） | 1-2 天 |
| **P4**（可选） | `CreateOIDCAuthRequestForMFA`、Request Object | — | 后续迭代 |

每阶段完成后单独 commit，保持可回滚粒度。

---

## 7. 风险与注意事项

| 风险 | 缓解 |
|---|---|
| 上游升级合并冲突 | 改动集中于 4 个既有文件的小段 + 2 个新文件；L2 保留 `SetOIDCService` 注入点，未来引入 enterprise 代码不冲突 |
| AGPLv3 合规 | 保留原版权头；修改后的完整源码随服务对外分发（AGPLv3 §13 网络交互条款） |
| TAI discovery/JWKS 非标准细节（issuer 带端口、JWK 未列 `kid` 等） | go-oidc 对常见偏差可容错；P1 阶段用 `tctl sso test`（内置 discovery 预检 + 诊断输出）最快暴露问题 |
| `user_name` 稳定性 | 需在 TAI 侧唯一且稳定（等价 `sub` 语义），否则同名用户会话互相覆盖；如不稳可改 `username_claim: sub` |
| OIDC connector CRUD 门控放行后 OSS 行为变化 | 仅影响自建 fork 的合规性认知：本 fork 明确启用 OIDC，属预期行为 |
| go.mod 依赖由 indirect 转 direct | `go mod tidy` 自动处理，版本不变，无供应链风险 |

---

## 8. 附录：TAI claims 与 Teleport 映射对照

| TAI claim | Teleport 映射 | 配置来源 |
|---|---|---|
| `user_name` | Teleport username | `spec.username_claim: user_name` |
| `sub` | `OIDCIdentity.ID`（外部身份标识，审计用） | 自动 |
| `name` | 不直接使用（可作 traits 供角色模板） | `claims_to_roles[].claim: name` |
| 任意 claim | traits（`external.<claim>`）+ roles 映射 | `claims_to_roles` |

---

## 9. 实现说明（与设计的差异）

| 项 | 设计 | 实现 | 原因 |
|---|---|---|---|
| claims→roles 匹配 | 新写 glob 匹配 | 复用 `services.TraitsToRoles` | 与 SAML/enterprise 语义一致（`*` 通配、`^...$` 正则、`$1` 展开），并产出 `tctl sso test` 所需 warnings |
| Auth HTTP 路由 | 未提及 | 新增 `POST /:version/oidc/requests/validate`（`lib/auth/apiserver.go`） | Proxy 通过 legacy HTTP API 调用 `ValidateOIDCAuthCallback`，OSS 未注册该路由（原由 enterprise 注册） |
| Web 路由注册路径 | `/v1/webapi/oidc/...` | `/webapi/oidc/...` | 路由器自动剥离 `/v1` 前缀，与 GitHub 路由写法一致；外部 URL 不变 |
| nonce | 未提及 | `nonce = state`；ID token 带 nonce 时必须匹配 | 防 ID token 重放；对不回传 nonce 的 IdP 保持兼容 |
| max_age / acr | 仅透传参数 | 透传并校验 `auth_time` / `acr` claim | 防止 IdP 忽略参数导致策略失效 |
| logins trait | “由 connector 名派生” | ID token 无 `logins` claim 时默认取用户名 | 与 GitHub 行为一致，`{{internal.logins}}` 开箱可用 |
| state 用后即删 | 用后即删 | 依赖 10 分钟 TTL 过期 | 后端无 `DeleteOIDCAuthRequest`，与 GitHub 行为一致；授权码本身在 IdP 侧一次性 |
| tctl | preset | preset `tai` + 新增 `--username-claim` 通用参数 | |
| 前端图标 | 加 TAI 关键词匹配 | 未改 | 无 TAI 图标素材，使用通用 OIDC 图标 |
| 代码组织 | 单文件 `lib/auth/oidc_auth.go` | 拆分为 `lib/oidcsso`（纯协议）+ `lib/auth/oidc_builtin.go`（接线）+ `lib/web/oidc_login.go` + tctl `oidc_tai_preset.go` | 便于跨版本复用；对上游文件的改动收敛为 ~6 个钩子点。详见 `TAI_OIDC_IMPLEMENTATION.md` |

