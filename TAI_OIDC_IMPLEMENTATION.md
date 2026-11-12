# Teleport 内建 OIDC（TAI 统一认证）— 实现与移植手册

> 适用读者：未来在新 Teleport 版本（如 `v18.11.10`）上**复现本功能**的工程师或 Claude Code。
> 设计背景与取舍见 `TAI_OIDC_DESIGN.md`；本文只讲**怎么做、怎么验、出问题怎么改**。
>
> 已验证基线：`v18.10.0`（commit `ddaa46b8`）。在该基线的干净 worktree 上执行 `tai-oidc/port.sh`，
> 补丁 3-way 全部干净应用，新增/修改文件与源分支逐字节一致，全部测试通过。

---

## 0. 快速路径（Claude Code 照做即可）

```bash
# 0) 前提：已有 feature/tai-oidc-login 分支（含本实现与 tai-oidc/ 工具包）
git fetch --tags
git checkout -b feature/tai-oidc-login-v18.11.10 v18.11.10

# 1) 一键移植：检查锚点 → 复制新文件 → 3-way 应用钩子补丁 → go.mod → 构建 → 测试期望补丁
git show feature/tai-oidc-login:tai-oidc/port.sh > /tmp/port.sh
bash /tmp/port.sh feature/tai-oidc-login

# 2) 验证（§6）
go test ./lib/oidcsso/... ./lib/modules/ ./tool/tctl/sso/... -count=1
go test ./lib/auth/ -run 'TestOIDC' -count=1
go test ./lib/web/ -run 'TestOIDC|TestGetWebConfig' -count=1
go build ./tool/teleport ./tool/tsh ./tool/tctl

# 3) 收尾（§7）：go mod tidy、刷新补丁基线、分阶段提交
go mod tidy
bash tai-oidc/gen-patches.sh v18.11.10
```

`port.sh` 任一步失败都会停下并指向本文对应章节：

| 失败步骤 | 去看 |
|---|---|
| 1/6 锚点检查 FAIL/WARN | §5 |
| 3/6 `hooks.patch` 冲突 | §4 |
| 5/6 编译失败（新文件引用的上游 API 变了） | §5.3 |
| 6/6 `tests.patch` 未应用 | §6.3 |

---

## 1. 背景速览（只列移植必须知道的事实）

1. 上游 OSS 只有 OIDC 的类型、存储、gRPC/HTTP 客户端、`tsh`/前端/`tctl sso test` 客户端流程；
   **协议实现、proxy web 路由、auth HTTP 校验路由**都在私有 enterprise 仓库。
2. OSS 通过三处把 OIDC 关掉：
   - `lib/auth/oidc.go`：`a.oidcAuthService == nil` 时返回 `errOIDCNotImplemented`；
   - `lib/modules/modules.go`：OSS `Features()` 不含 `entitlements.OIDC`，
     `lib/auth/auth_with_roles.go` 中 4 处 `GetEntitlement(entitlements.OIDC)` 门控因此拒绝；
   - proxy / auth HTTP 均未注册 OIDC 路由。
3. 本实现补齐上述缺口，且**保留 `SetOIDCService` 注入点**：一旦注入 enterprise 实现，自动让位。
4. TAI 的唯一特殊点是用户名在 `user_name` claim —— 纯配置（`username_claim: user_name`），代码不含任何 TAI 硬编码；
   `tctl` 的 `tai` 预设只是帮你填好这一项和 scope。

---

## 2. 代码结构

### 2.1 分层与依赖方向

```
                 ┌───────────────────────────────────────────────┐
  可跨版本复用   │ lib/oidcsso            纯协议（零 auth/web 依赖） │
  （极少改动）   │   oidcsso.go           discovery 缓存 / 授权 URL │
                 │                        code 换 token / ID token 校验│
                 │                        claims→username/traits/roles│
                 │ lib/oidcsso/oidcssotest 假 IdP（测试夹具）       │
                 └───────────────▲───────────────────────▲───────┘
                                 │                       │
  随上游演进     ┌───────────────┴──────────┐  ┌─────────┴─────────────┐
  （对照 GitHub  │ lib/auth/oidc_builtin.go │  │ lib/web/oidc_login.go │
   SSO 同步）    │ 存储/用户/会话/证书/审计 │  │ 3 个 proxy handler    │
                 │ /诊断 + HTTP 校验 handler│  │ + bindOIDCLoginHandlers│
                 └───────────────▲──────────┘  └─────────▲─────────────┘
                                 │ 钩子（hooks.patch）     │
  上游文件       oidc.go · auth.go · apiserver.go · modules.go · web/apiserver.go · tctl oidc.go
```

依赖规则（修改时必须保持）：

- `lib/oidcsso` **只能**依赖 `api/types`、`lib/services`、`lib/utils`、`lib/defaults`、`teleport` 根包、go-oidc/oauth2。
  不得 import `lib/auth`、`lib/web`（会产生循环依赖，也破坏可复用性）。
- 版本相关的逻辑（会话、证书、审计、`ServerWithRoles`、web 辅助函数）一律放在胶水层。

### 2.2 文件清单（`tai-oidc/files.txt`，移植时原样复制）

| 文件 | 行数 | 职责 | 版本敏感度 |
|---|---|---|---|
| `lib/oidcsso/oidcsso.go` | ~390 | `ProviderCache`、`OAuth2Config`、`AuthCodeURL`、`ExchangeAndVerify`、`CheckAuthTime/CheckACR/CheckEmailVerified`、`UsernameFromClaims`、`ClaimsToTraits`、`MapRoles`、`IdentityFromClaims`、`ErrNoRoles` | 低 |
| `lib/oidcsso/oidcsso_test.go` | ~190 | 纯函数单测 + 对假 IdP 的协议往返 | 低 |
| `lib/oidcsso/oidcssotest/idp.go` | ~230 | 假 TAI IdP：discovery/JWKS/token；`ConnectorSpec()`、`PrepareLogin()`、`WithMismatchedKeys()` | 低 |
| `lib/auth/oidc_builtin.go` | ~610 | `getOIDCService()`（enterprise 优先 + 懒加载内建）、`builtinOIDCService`（Create/Validate/ForMFA）、用户计算与落库、web session/证书、审计事件、SSO 诊断、`APIServer.validateOIDCAuthCallback` | **高**（结构仿 `lib/auth/github.go`） |
| `lib/auth/oidc_builtin_test.go` | ~460 | auth 层端到端（11 个测试函数，`TestOIDCLoginFailures` 含 10 个负例子测试） | 中（依赖 `newTestTLSServer`、`newSSHAndTLSKeyPairs`） |
| `lib/web/oidc_login.go` | ~210 | `bindOIDCLoginHandlers` + `oidcLoginWeb/oidcLoginConsole/oidcCallback` | **高**（结构仿 GitHub handler） |
| `lib/web/oidc_login_test.go` | ~190 | proxy→auth HTTP→假 IdP 全链路（tsh 与浏览器） | 中（依赖 `newWebPack`、`sshLoginResponseFromCallbackResponse`） |
| `tool/tctl/sso/configure/oidc_tai_preset.go` | ~60 | `tai` 预设，`init()` 追加到 `oidcPresets` | 低 |
| `docs/.../integrate-idp/tai.mdx` | ~105 | 用户接入指南 | 低 |

### 2.3 工具包（`tai-oidc/`）

| 文件 | 用途 |
|---|---|
| `files.txt` | 新文件清单（`port.sh` 据此复制） |
| `hook-files.txt` | 被钩子修改的上游文件清单（`gen-patches.sh` 据此生成补丁） |
| `hooks.patch` | 6 个上游文件的最小改动（+24 行） |
| `tests.patch` | `lib/web/apiserver_test.go` 中 webconfig 期望值调整（+4 行） |
| `BASE` | 补丁生成时的上游基线 tag |
| `check-upstream.sh` | 在目标树中检查锚点、门控、新文件依赖的上游 API |
| `port.sh` | 一键移植（源可以是 git ref 或目录） |
| `gen-patches.sh` | 移植完成后重新生成补丁，供下次移植 |

---

## 3. 钩子点详解（`hooks.patch` 的逐项说明）

补丁冲突时**按本节手工重做**，不要整段覆盖上游文件。

### H1 `lib/auth/oidc.go` — 默认接线
- **锚点**：`func (a *Server) CreateOIDCAuthRequest(` / `CreateOIDCAuthRequestForMFA(` / `ValidateOIDCAuthCallback(`，以及 `var errOIDCNotImplemented`。
- **改动**：删除 `errOIDCNotImplemented`；三个方法体改为
  ```go
  svc, err := a.getOIDCService()   // 定义在 oidc_builtin.go
  if err != nil {
      return nil, trace.Wrap(err)
  }
  rq, err := svc.CreateOIDCAuthRequest(ctx, req)   // 另两个同理
  return rq, trace.Wrap(err)
  ```
- **不变量**：`a.oidcAuthService != nil` 时必须走注入实现（`TestOIDCInjectedServiceTakesPrecedence` 守护）。
- 若上游新增了第 4 个委托方法：同样改为经 `getOIDCService()` 转发，并在 `builtinOIDCService` 上实现（不支持则返回 `trace.NotImplemented`）。

### H2 `lib/auth/auth.go` — Server 状态字段
- **锚点**：`Server` 结构体中的 `oidcAuthService OIDCService`。
- **改动**：其后加一行字段（类型定义在 `oidc_builtin.go`）：
  ```go
  // oidcBuiltin holds the built-in OIDC service used when no
  // oidcAuthService has been injected. See oidc_builtin.go.
  oidcBuiltin oidcBuiltinState
  ```
- 依赖：`Server.closeCtx`、`Server.clock`、`Server.logger`（用于 provider 缓存生命周期与日志）。

### H3 `lib/auth/apiserver.go` — auth HTTP 校验路由（设计文档遗漏项）
- **为什么必须**：proxy 调用 `authclient.HTTPClient.ValidateOIDCAuthCallback`，走 legacy HTTP
  `POST /:version/oidc/requests/validate`；OSS 未注册该路由（原由 enterprise 注册），缺失则回调必然失败。
- **锚点**：`srv.POST("/:version/github/requests/validate", ...)`。
- **改动**：其后加一行
  ```go
  srv.POST("/:version/oidc/requests/validate", srv.WithAuth(srv.validateOIDCAuthCallback))
  ```
  handler 本体在 `oidc_builtin.go`。
- **未来**：若上游把 `ValidateOIDCAuthCallback` 客户端改为 gRPC（检查 `lib/auth/authclient/http_client.go` 是否仍有
  `c.Endpoint("oidc", "requests", "validate")`），则删除本钩子与 handler，改为确认 gRPC 服务端已转发到 `Server.ValidateOIDCAuthCallback`。

### H4 `lib/modules/modules.go` — entitlement 放行
- **锚点**：`func (p *defaultModules) Features() Features` 的 `Entitlements` map（`entitlements.K8s:` 行）。
- **改动**：加入 `entitlements.OIDC: {Enabled: true, Limit: 0},`（附注释）。
- 效果：`auth_with_roles.go` 中所有 `GetEntitlement(entitlements.OIDC)` 门控放行（v18.10.0 为 4 处）；
  webconfig 中 `OIDC` 布尔与 `entitlements["OIDC"]` 变为 true（见 §6.3）。

### H5 `lib/web/apiserver.go` — proxy 路由
- **锚点**：`h.POST("/webapi/github/login/console", ...)`（`bindDefaultEndpoints` 内）。
- **改动**：其后加
  ```go
  // OIDC connector handlers (built-in OIDC, see oidc_login.go)
  h.bindOIDCLoginHandlers()
  ```
- 路径（由既有客户端固定，不可改）：`/webapi/oidc/login/web`（前端按钮）、`/webapi/oidc/callback`（IdP 回调）、
  `/webapi/oidc/login/console`（tsh）。路由器自动接受 `/v1` 前缀，故注册时不带 `/v1`。

### H6 `tool/tctl/sso/configure/oidc.go` — 通用 flag
- **锚点**：`sub.Flag("acr", ...)`。
- **改动**：其后加 `--username-claim` flag（写入 `spec.UsernameClaim`）。
- `tai` 预设本身在新文件中通过 `init()` 追加，不改此文件的预设列表。

### H7 `go.mod`（`port.sh` 用 sed 处理，不在补丁里）
- 把 `github.com/coreos/go-oidc/v3 vX // indirect` 的 `// indirect` 去掉；随后 `go mod tidy` 会把它移到直接依赖区。
- **不要升级版本**：沿用目标版本 go.mod 中已有的 go-oidc 版本。若目标版本已不依赖 go-oidc：
  `go get github.com/coreos/go-oidc/v3@v3.17.0`（或更新的兼容版本）。

---

## 4. 补丁冲突处理

`git apply --3way` 失败时：

1. `git status` 查看冲突文件；冲突只可能出现在 §3 的 6 个文件里。
2. 每个文件都只有 1–3 行语义改动，按 §3 对应条目在**目标版本的新位置**手工重做；锚点挪动、改名时按语义找等价位置：
   - H3/H5：找“GitHub SSO 路由注册”的位置，OIDC 路由紧随其后；
   - H4：找 OSS `defaultModules` 的 entitlement map；
   - H1：找 `OIDCService` 委托方法。
3. 解决后 `git diff` 自查：除注释外，上游文件的新增行数应与 `hooks.patch` 相当（~24 行）。

---

## 5. `check-upstream.sh` 报告的适配指南

退出码：`0` 全部通过；`1` 有 FAIL；`3` 目标树已包含本移植（`port.sh` 会直接结束）。

### 5.1 Hook anchors FAIL
按 §3/§4 在新位置手工实现钩子。特别地：
- `errOIDCNotImplemented` 缺失：上游可能改了 OSS 的拒绝方式。**先确认上游是否已提供 OSS OIDC 实现**
  （`grep -rn "OIDCService" lib/auth/*.go`、`git log v18.10.0..HEAD -- lib/auth/oidc.go`），若是，评估是否还需要本移植。
- `"/:version/oidc/requests/validate"` WARN（已存在）：上游或此前移植已注册，跳过 H3。
- `"/webapi/oidc/login/web"` WARN（已存在）：上游已有 OIDC web 路由，跳过 H5，并比较行为差异。

### 5.2 Enterprise gating WARN
- `entitlements.OIDC` 出现在新的非测试文件：逐一确认 H4 能覆盖（同样读取 `GetEntitlement(entitlements.OIDC)` 即可）；
  若新门控用的是 `BuildType() != modules.BuildEnterprise` 等其他判断，需要额外放行并在本文 §3 追加一个钩子条目。

### 5.3 Upstream APIs FAIL 或编译错误
新文件依赖的上游符号（`check-upstream.sh` 第三段逐项检查）：

| 符号 | 被谁用 | 变化时的处理 |
|---|---|---|
| `types.OIDCConnector` 的 `GetUsernameClaim/GetTraitMappings/GetMaxAge/IsPKCEEnabled/GetClientRedirectSettings/GetAllowUnverifiedEmail/GetPrompt/GetACR/GetScope` | `oidcsso` | 对齐新签名；语义不变 |
| `types.OIDCAuthRequest` 字段 `PkceVerifier/LoginHint/Scope/ProxyAddress/*AttestationStatement` | 胶水层 | 字段改名则对齐；新增字段参照 GitHub 是否透传 |
| `types.SSODiagnosticInfo` 的 `OIDC*` 字段 | 胶水层 | 对齐 |
| `services.GetRedirectURL`、`services.TraitsToRoles`、`services.FetchRolesWithContext` | 两层 | 对齐签名 |
| `sso.ValidateClientRedirect`、`sso.CeremonyType*` | 胶水层 | 参照 `github.go` 当前用法 |
| `NewWebSessionRequest`、`SessionCertsRequest`、`CreateSessionCerts`、`ClientOptionsForLogin`、`CallLoginHooks`、`GetUserOrLoginState`、`GetLoginRuleEvaluator` | `oidc_builtin.go` | **对照 `github.go`**（§5.4） |
| `authclient.OIDCAuthRawResponse`、`ValidateOIDCAuthCallbackReq` | HTTP handler | 对齐 |
| `ParseSSORequestParams`、`SSOSetWebSessionAndRedirectURL`、`ConstructSSHResponse`、`AuthParams`、`RedirectURLWithError`、`BuildDeviceWebRedirectPath` | `oidc_login.go` | **对照 GitHub handler**（§5.4） |
| 测试辅助：`newTestTLSServer`、`newSSHAndTLSKeyPairs`（auth_test 包）；`newWebPack`、`sshLoginResponseFromCallbackResponse`、`testProxy.web/webURL/newClient`（web 包） | 测试 | 对齐 |

### 5.4 关键方法：用 GitHub SSO 的上游差异驱动同步

`oidc_builtin.go` 与 `oidc_login.go` 是按 GitHub SSO 逐段仿写的。上游对 SSO 公共流程的改动（新增会话字段、证书参数、
审计字段、设备信任、scope 等）几乎总会先体现在 GitHub 实现上。移植时执行：

```bash
OLD=v18.10.0; NEW=v18.11.10
git diff $OLD $NEW -- lib/auth/github.go
git diff $OLD $NEW -- lib/web/apiserver.go | grep -n -A40 -B5 'github'   # 只看 githubLoginWeb/Console/Callback
git diff $OLD $NEW -- lib/auth/authclient/http_client.go lib/auth/oidc.go api/types/oidc.go lib/client/sso.go lib/client/weblogin.go
```

对应关系：

| GitHub（上游） | OIDC（本实现） |
|---|---|
| `Server.CreateGithubAuthRequest` | `builtinOIDCService.CreateOIDCAuthRequest` |
| `validateGithubAuthCallbackHelper`（审计事件） | `builtinOIDCService.ValidateOIDCAuthCallback` |
| `Server.ValidateGithubAuthRedirect` | `builtinOIDCService.validateOIDCAuthCallback` |
| `Server.calculateGithubUser` | `builtinOIDCService.calculateOIDCUser` |
| `Server.createGithubUser` | `builtinOIDCService.createOIDCUser` |
| `Server.makeGithubAuthResponse` | `builtinOIDCService.makeOIDCAuthResponse` |
| `APIServer.validateGithubAuthCallback` | `APIServer.validateOIDCAuthCallback` |
| `Handler.githubLoginWeb/githubLoginConsole/githubCallback` | `Handler.oidcLoginWeb/oidcLoginConsole/oidcCallback` |

规则：GitHub 侧出现的**通用**改动（新请求字段透传、`NewWebSessionRequest`/`SessionCertsRequest` 新参数、审计事件新字段、
回调重定向逻辑），等价地搬到 OIDC 对应函数；GitHub 专有逻辑（teams、org SSO 检查、API 客户端）忽略。

---

## 6. 验证

### 6.1 自动化测试（必须全绿）

| 命令 | 覆盖 |
|---|---|
| `go test ./lib/oidcsso/... -count=1` | 用户名提取/规范化、claims→traits、claims_to_roles（glob/正则/`$1`）、email_verified、max_age、acr、授权 URL + code 交换 + ID token 校验往返 |
| `go test ./lib/auth/ -run TestOIDC -count=1` | 授权 URL 参数（scope/nonce/prompt/login_hint/PKCE）、回调白名单、web 登录（用户/traits/角色/审计）、二次登录更新、拒绝覆盖本地用户、tsh 证书签发、PKCE S256、`tctl sso test` 干跑与诊断落库、enterprise 优先；负例：缺 claim、非字符串 claim、无角色、aud/iss 错、过期、nonce 错、签名错、email 未验证、max_age 超时、IdP error、未知 state、错误 code |
| `go test ./lib/web/ -run 'TestOIDC\|TestGetWebConfig' -count=1` | proxy 全链路：`/webapi/oidc/login/console` → IdP → `/webapi/oidc/callback` → auth HTTP 校验 → 解密 CLILoginResponse；浏览器 302 + CSRF + session cookie；非法回调 URL；webconfig 期望 |
| `go test ./lib/modules/ ./tool/tctl/sso/... -count=1` | entitlement 与 tctl 预设 |
| `go build ./tool/teleport ./tool/tsh ./tool/tctl` | 三个二进制可构建 |

回归（建议，耗时较长）：`go test ./lib/auth/ ./lib/web/ ./lib/services/ ./tool/tctl/... -count=1`。
已知 `TestNotificationPagination` 偶发失败，与本功能无关（单独重跑通过）。

### 6.2 冒烟检查
```bash
go build -o /tmp/tctl ./tool/tctl && /tmp/tctl sso configure oidc --help | grep -E 'tai|username-claim'
# 预期：预设列表含 tai；存在 --username-claim
```

### 6.3 测试期望的版本差异（`tests.patch`）
H4 让 OSS webconfig 的 OIDC 变为启用，`TestGetWebConfig_WithEntitlements` 需要：
- OSS 段期望：`OIDC: true`，`entitlements[OIDC] = {Enabled: true}`；
- 切换到自定义 test modules 后的段：`expectedCfg.OIDC = false`、`expectedCfg.Entitlements[OIDC] = {Enabled: false}`。

`tests.patch` 未能应用时按上述语义手改；其他 webconfig 测试若用自定义 modules，通常无需改。

### 6.4 对真实 TAI 的验收（人工）
1. `tctl sso configure oidc --preset tai --issuer-url ... --id ... --secret ... --redirect-url https://<proxy>/v1/webapi/oidc/callback -r 'user_name,*,access' > tai.yaml`
2. `tctl sso test tai.yaml` → 输出中应看到 `[OIDC] Claims`（含 `user_name`）、`Calculated identity`、角色 `access`，且显示成功。
3. `tctl create -f tai.yaml`；Web UI 出现 TAI 按钮并能登录；`tsh login --proxy=<proxy> --auth=tai` 成功签发证书。
4. 审计日志出现 `user.login`（method `oidc`）。

---

## 7. 收尾与提交

1. `go mod tidy`（把 go-oidc 移入直接依赖区；确认 diff 只涉及该依赖）。
2. 刷新工具包供下次移植：`bash tai-oidc/gen-patches.sh <新基线tag>`（会更新 `hooks.patch`、`tests.patch`、`BASE`）。
3. 若本次移植新增/调整了钩子，同步更新 §3、`hook-files.txt`、`check-upstream.sh`。
4. 建议提交粒度：
   1. `oidcsso: add OIDC relying-party package and fake IdP`（`lib/oidcsso/**`、`go.mod/go.sum`）
   2. `auth: built-in OIDC service and validate route`（`lib/auth/oidc_builtin*.go`、H1–H3）
   3. `modules: enable OIDC entitlement in OSS`（H4 + `tests.patch`）
   4. `web: OIDC login handlers`（`lib/web/oidc_login*.go`、H5）
   5. `tctl: tai preset and --username-claim`（预设文件、H6）
   6. `docs/tai-oidc: TAI guide and porting kit`

---

## 8. 必须保持的行为不变量（改代码时的红线）

安全相关（`oidc_builtin_test.go` / `oidcsso_test.go` 中均有用例守护，**不得删除或放宽这些测试**）：

1. ID token 必须经 go-oidc `Verifier` 校验签名、`iss`、`aud == client_id`、`exp`；不得解析未验签 payload，不用 userinfo。
2. `state` 为 `CryptoRandomHex(TokenLenBytes)`，存储 TTL `defaults.OIDCAuthRequestTTL`；`nonce = state`，ID token 带 nonce 时必须一致。
3. 非 web-session 请求（tsh / test flow）必须过 `sso.ValidateClientRedirect`（防开放重定向）。
4. 只有 proxy 能创建 web session —— 依赖上游 `ServerWithRoles` 中既有检查，勿绕过 `ServerWithRoles` 直接调用。
5. `email_verified == false` 时拒绝（除非 `allow_unverified_email`）；`max_age` / `acr_values` 配置时校验 `auth_time` / `acr`。
6. 不得覆盖非 OIDC 创建的同名用户（`IsSameProvider` 检查）。
7. `client_secret` 与授权码不得写入日志/审计/诊断（proxy 回调日志刻意**不**打印 query）。
8. `SSOTestFlow` 为干跑：不落库用户、不签发证书，只写诊断。
9. enterprise 注入优先（`SetOIDCService`）。

行为约定（可按需求调整，但需同步测试与 `tai.mdx`）：
- 用户名 = `username_claim`（默认 `email`）的值，去首尾空白并转小写，长度 ≤ `teleport.MaxUsernameLength`。
- traits = 所有标量/数组 claim（嵌套对象忽略）；无 `logins` claim 时 `logins = [username]`；之后执行 Login Rules。
- 角色 = `services.TraitsToRoles(connector.GetTraitMappings(), traits)`；为空返回 `ErrOIDCNoRoles`（AccessDenied）。注意：错误经 auth HTTP 边界后 `errors.Is` 不再成立，proxy 实际显示通用的回调失败页（与 GitHub 的 `ErrGithubNoTeams` 行为相同）。
- scope 总含 `openid`；connector 未配其他 scope 时补 `email`。
- PKCE：connector `pkce_mode: enabled` 时启用，优先使用 tsh 传来的 verifier，否则服务端生成。
- `CreateOIDCAuthRequestForMFA` 返回 `NotImplemented`。

---

## 9. 故障排查

| 现象 | 原因 / 处理 |
|---|---|
| `OIDC is only available in Teleport Enterprise` | H4 未生效（entitlement 未启用）或有新门控（§5.2） |
| `OIDC is only available in enterprise subscriptions` | H1 未生效（仍是上游 `errOIDCNotImplemented`） |
| 回调页 “login failed / bad callback”，auth 日志有 404/`not found` 于 `/oidc/requests/validate` | H3 缺失 |
| 点击按钮 404 | H5 缺失 |
| `failed to discover OIDC provider` | auth 服务无法访问 `<issuer>/.well-known/openid-configuration`；或 discovery 中 `issuer` 与 `issuer_url` 不完全一致（含尾斜杠、端口） |
| `failed to verify ID token: ... issuer did not match` | 同上，`issuer_url` 必须与 ID token `iss` 逐字一致 |
| `... audience ...` | TAI 签发的 `aud` 不是 connector 的 `client_id` |
| IdP 报 `redirect_uri` 不匹配 | connector `redirect_url` 与 TAI 后台登记值不一致（需为 `https://<proxy公网地址>/v1/webapi/oidc/callback`） |
| `ID token is missing the "user_name" claim` | TAI 未在 ID token 返回 `user_name`；检查 scope（`profile`）或改 `username_claim` |
| `unable to map claims to any role` | `claims_to_roles` 无命中；用 `tctl sso test` 查看 claims 与 warnings |
| `local user "x" already exists and is not an OIDC user` | 存在同名本地用户；改名或删除本地用户 |
| `invalid or disallowed client redirect URL` | tsh 回调地址不合规；检查 connector `client_redirect_settings` |

---

## 10. 给 Claude Code 的移植提示词模板

```
在当前仓库（Teleport <目标版本>）复现 TAI OIDC 内建登录。
严格按 TAI_OIDC_IMPLEMENTATION.md 执行（先用 `git show feature/tai-oidc-login:TAI_OIDC_IMPLEMENTATION.md` 阅读）：
1. 从 <目标tag> 建分支，运行 port.sh（§0）。
2. 若 port.sh 失败，按 §4/§5 适配；用 §5.4 的 GitHub diff 方法同步 oidc_builtin.go 与 oidc_login.go。
3. 不得放宽 §8 的不变量与其测试。
4. 跑完 §6.1 全部测试与构建，报告结果（含失败输出）。
5. 按 §7 刷新 tai-oidc 补丁并更新本文档中与新版本不符的描述；未经确认不要提交。
```
