# TAI OIDC 登录配置手册

本文说明如何在本 fork 构建的 Teleport（带内建 OIDC）上配置 TAI 统一认证登录，以及如何控制哪些 TAI 用户能登录、拿到什么权限。

- 实现原理与移植：见 `TAI_OIDC_IMPLEMENTATION.md`、`TAI_OIDC_DESIGN.md`
- 面向最终用户的接入说明：见 `docs/pages/zero-trust-access/sso/integrate-idp/tai.mdx`

文中以当前部署为例：

| 项 | 值 |
|---|---|
| Teleport 公网地址 | `teleport.agpl.oitcep.com:443` |
| TAI issuer | `https://tai.it.tencent.com/api/auth-center/oauth2/` |
| TAI client_id | `teleport-agpl` |
| 回调地址 | `https://teleport.agpl.oitcep.com/v1/webapi/oidc/callback` |
| 连接器名称 | `tai` |

`client_secret` 只保存在 Teleport 后端和 TAI 后台，不要写进本文、仓库或聊天记录。

---

## 1. 前置条件

1. **Teleport 使用本 fork 构建的二进制**。确认 OIDC 已启用：

   ```bash
   teleport version          # 应为 feature/tai-oidc-login 分支的构建
   curl -s https://teleport.agpl.oitcep.com/web/config.js | grep -o '"oidc":[a-z]*'
   # 预期输出 "oidc":true
   ```

2. **Proxy 的 `public_addr` 必须是用户实际访问的域名**，回调地址由它拼出。`/etc/teleport.yaml` 的相关部分：

   ```yaml
   proxy_service:
     enabled: "yes"
     web_listen_addr: 0.0.0.0:443
     public_addr: teleport.agpl.oitcep.com:443
     https_keypairs:
     - key_file: /etc/letsencrypt/live/teleport.agpl.oitcep.com/privkey.pem
       cert_file: /etc/letsencrypt/live/teleport.agpl.oitcep.com/fullchain.pem
     https_keypairs_reload_interval: 1h   # certbot 续期后自动加载新证书
   ```

3. **Auth Service 能访问 TAI**：`<issuer>/.well-known/openid-configuration` 及其中的 JWKS 地址。

   ```bash
   curl -s https://tai.it.tencent.com/api/auth-center/oauth2/.well-known/openid-configuration | head -c 300
   ```

## 2. 在 TAI 后台登记应用

1. 新建（或使用已有的）OIDC 应用，记下 `client_id` 和 `client_secret`。
2. 回调地址填：`https://teleport.agpl.oitcep.com/v1/webapi/oidc/callback`（必须逐字一致，含 `/v1`）。
3. 确认 ID token 中带有 `user_name` claim（需要 `profile` scope），Teleport 用它作为用户名。

## 3. 创建连接器

### 3.1 用 tctl 预设生成

`tai` 预设会自动设置 `username_claim: user_name` 和 `scope: [openid, profile]`。

```bash
umask 077
tctl sso configure oidc --preset tai \
  --issuer-url https://tai.it.tencent.com/api/auth-center/oauth2/ \
  --id teleport-agpl \
  --secret '<CLIENT_SECRET>' \
  --redirect-url https://teleport.agpl.oitcep.com/v1/webapi/oidc/callback \
  -r 'user_name,lideding,editor,access,auditor' \
  -r 'user_name,^(alice|bob)$,access' \
  > /root/tai.yaml
```

`-r` 的格式是 `claim名,claim值,角色1,角色2,...`，可重复。角色映射的写法见第 4 节。

> 注意：`--secret` 会出现在 shell 历史里。执行后可用 `history -d` 删除，或先 `export HISTCONTROL=ignorespace` 并在命令前加一个空格。

### 3.2 连接器字段说明

```yaml
kind: oidc
version: v3
metadata:
  name: tai                       # 连接器 ID，tsh login --auth=tai 用的就是它
spec:
  display: TAI                    # Web 登录页按钮文字
  issuer_url: https://tai.it.tencent.com/api/auth-center/oauth2/   # 必须与 ID token 的 iss 逐字一致（含末尾斜杠）
  client_id: teleport-agpl        # 同时也是 ID token aud 的期望值
  client_secret: <CLIENT_SECRET>
  redirect_url: https://teleport.agpl.oitcep.com/v1/webapi/oidc/callback
  scope: [openid, profile]
  username_claim: user_name       # Teleport 用户名取自该 claim（去空白、转小写）
  claims_to_roles:                # 授权规则，见第 4 节
  - claim: user_name
    value: lideding
    roles: [editor, access, auditor]
```

其他可选字段：`prompt`（如 `login`）、`max_age`、`acr_values`、`pkce_mode: enabled`、`allow_unverified_email`。

### 3.3 先测试再生效

```bash
tctl sso test /root/tai.yaml   # 浏览器走一遍 TAI 登录，不建用户、不发证书，输出 claims 和算出的角色
tctl create --force /root/tai.yaml   # 创建；已存在时覆盖
shred -u /root/tai.yaml        # 文件里有 secret，用完删掉
```

创建后，Web 登录页会出现 “TAI” 按钮；命令行登录：

```bash
tsh login --proxy=teleport.agpl.oitcep.com --auth=tai
```

---

## 4. 授权：谁能登录、拿到什么角色

### 4.1 规则是怎么生效的

- 每次 TAI 登录时，Teleport 把 ID token 的 claims 转成 traits，再用连接器的 `claims_to_roles` **逐条匹配**，所有命中规则的角色**取并集**。
- **一条都没命中 → 拒绝登录**（日志：`unable to map claims to any role`）。
- 用户记录在每次登录时按规则**重新生成**，所以：
  - 在 Teleport 里直接改 SSO 用户的角色（`tctl users update`）没有用，下次登录就被覆盖；
  - **授权只改连接器的 `claims_to_roles`**。

### 4.2 不要用通配规则

```yaml
# 危险：任何能登录 TAI 的人都会拿到 access 角色
- claim: user_name
  value: '*'
  roles: [access]
```

`access` 角色允许以 `{{internal.logins}}`（默认即 TAI 用户名）登录所有节点。TAI 是公司级身份源，通配等于把集群开放给全公司。应改为白名单。

### 4.3 白名单写法

`value` 支持三种形式：精确值、`*` 通配、`^...$` 正则（可用 `$1` 引用捕获组）。

**逐用户一条**（适合人少、各人角色不同）：

```yaml
claims_to_roles:
- claim: user_name
  value: lideding
  roles: [editor, access, auditor, tai-superadmin]
- claim: user_name
  value: alice
  roles: [access]
```

**按角色分组，一条正则列出成员**（适合人多）：

```yaml
claims_to_roles:
- claim: user_name
  value: '^(lideding)$'
  roles: [editor, access, auditor, tai-superadmin]
- claim: user_name
  value: '^(alice|bob|carol)$'
  roles: [access]
```

正则务必带 `^` 和 `$`，否则 `bob` 会匹配到 `bobby`。

如果 TAI 以后在 ID token 里提供部门/组之类的 claim，也可以按它映射（例如 `claim: groups, value: teleport-users`），这样在 TAI 侧维护成员即可。当前 TAI 的 ID token 中没有组信息，只能按 `user_name` 白名单。

### 4.4 超级管理员

Teleport 内置的管理员角色组合是 `editor`（管理用户、角色、连接器等配置）+ `access`（访问资源）+ `auditor`（审计日志、会话录像）。

`access` 只允许用 TAI 用户名作为主机登录名。若管理员需要 `root` 登录，额外创建一个角色：

```yaml
kind: role
version: v8
metadata:
  name: tai-superadmin
spec:
  allow:
    logins: [root, '{{internal.logins}}']
    node_labels:
      '*': '*'
```

```bash
tctl create -f tai-superadmin.yaml
```

再在 `claims_to_roles` 中给管理员加上 `tai-superadmin`（见 4.3 示例）。

### 4.5 日常操作：增删用户

连接器里有 secret，修改时要连 secret 一起导出，否则覆盖后 secret 会丢失：

```bash
umask 077
F=$(mktemp /tmp/tai-oidc.XXXXXX.yaml)
tctl get oidc/tai --with-secrets > "$F"
vi "$F"                         # 修改 claims_to_roles
grep -q client_secret "$F" && tctl create --force "$F"
shred -u "$F"
tctl get oidc/tai | sed -n '/claims_to_roles/,/client_id/p'   # 确认结果
```

- **新增用户 / 改角色**：用户下次登录时生效（已登录的需要重新登录）。
- **移除用户**：从规则里删掉后，该用户**无法再次登录**，但已经签发的证书和 Web 会话在过期前仍然有效（默认 12 小时）。需要立刻踢出时：

  ```bash
  tctl lock --user=<用户名> --message="access revoked" --ttl=24h   # 立即使其现有证书和会话失效
  tctl users rm <用户名>                                           # 可选：删除已落库的用户记录
  ```

---

## 5. 验证

```bash
tctl get oidc/tai | grep -v client_secret    # 查看连接器（不带 secret）
tctl get user/<用户名> | grep -A6 'roles:'    # 登录后查看用户实际角色
tsh status                                   # 用户侧查看自己的角色和可用登录名
grep 'user.login' /var/log/teleport.log | tail   # 审计：method:oidc success:true/false
```

未在白名单中的用户登录时，应看到 Web 端登录失败页，auth 日志出现 `unable to map claims to any role`。

---

## 6. 常见问题

| 现象 | 原因 / 处理 |
|---|---|
| 登录页没有 TAI 按钮 | 连接器未创建（`tctl get oidc`），或二进制不是本 fork 构建 |
| TAI 报 `redirect_uri` 不匹配 | 连接器 `redirect_url` 与 TAI 后台登记值不一致 |
| `failed to discover OIDC provider` | 本机访问不到 TAI discovery 地址，或 `issuer_url` 与 discovery 中的 `issuer` 不一致（注意末尾斜杠） |
| `issuer did not match` / `audience` 错误 | `issuer_url` 与 ID token `iss` 不一致；或 `client_id` 与 `aud` 不一致 |
| `ID token is missing the "user_name" claim` | scope 缺 `profile`，或 TAI 应用未返回该 claim |
| `unable to map claims to any role` | 用户不在白名单（预期行为）；或规则写错，用 `tctl sso test` 查看 claims 与 warnings |
| `local user "x" already exists and is not an OIDC user` | 存在同名本地用户（如用 `tctl users add` 建的），删除本地用户或改名 |
| 改了角色没生效 | 用户需重新登录；或改的是用户而不是连接器（见 4.1） |
| 更新连接器后 TAI 登录全部失败 | 覆盖时丢了 `client_secret`，按 4.5 带 `--with-secrets` 重新导出修改 |

更多细节见 `TAI_OIDC_IMPLEMENTATION.md` §9。