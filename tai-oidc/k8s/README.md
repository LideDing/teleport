# 在 Kubernetes 中用 Helm 部署自编译的 Teleport（TAI OIDC）

本目录：

| 文件 | 用途 |
|---|---|
| `values.yaml` | 官方 `teleport-cluster` chart 的覆盖值，指向自编译镜像 |
| `README.md` | 本文 |

镜像构建：`build.assets/charts/Dockerfile-distroless-local` + `make image-local`。
Chart：直接使用仓库内 `examples/chart/teleport-cluster`（与二进制同一提交），不改模板。

---

## 1. 官方 chart 评估

结论：**官方 `teleport-cluster` chart 满足需求，只需覆盖 values，不需要 fork 模板。**

| 关注点 | chart 现状 | 处理 |
|---|---|---|
| 镜像来源 | 默认 `public.ecr.aws/gravitational/teleport-distroless`（官方发布的二进制） | `image` 改为自编译镜像；`enterpriseImage` 也指向自编译镜像，防止误开 `enterprise` 时拉官方商业版 |
| 所有用到镜像的地方 | auth、proxy、proxy 的 `wait-auth-update` init 容器、两个部署前配置校验 Job，全部取 `image` | 渲染结果已核对：只出现自编译镜像，没有 `gravitational` / `public.ecr.aws` / license |
| 商业版 | `enterprise: false` 时不挂 license、不用商业版镜像 | 保持 `false` |
| Operator 子 chart | 默认关闭，镜像固定为官方 `teleport-operator` | 保持关闭（自编译版本没有 operator 镜像） |
| OIDC | values 注释写 “oidc 仅 Enterprise”，但模板不做校验，只是写入 `authentication.type` | 本 fork 的二进制已内建 OIDC，可直接用 `type: oidc` |
| 仅允许 TAI 登录 | `authentication.type/connectorName/localAuth` 直接映射到 `auth_service.authentication` | `type: oidc`、`connectorName: tai`、`localAuth: false`（见 5.1） |
| 连接器管理 | chart 只能通过 operator 声明 SSO 连接器；`--apply-on-startup` 不支持 `oidc` 类型 | 部署后用 `kubectl exec … tctl` 创建（第 5 节） |
| TLS | `highAvailability.certManager` 生成 cert-manager `Certificate`（secret 固定为 `teleport-tls`，域名固定为 `<clusterName>` + `*.<clusterName>`），proxy 每 12h 重新读取 secret | 使用集群已有的 cert-manager；issuer 必须支持 DNS-01（通配证书） |
| DNS | chart 无专门开关，但可给 Service 加注解 | 用 `proxy.annotations.service` 加 external-dns 注解，只作用于 proxy 的 LoadBalancer Service |
| 版本号 | 镜像 tag 取 `teleportVersionOverride`，必须是合法 semver（chart 要算主版本） | tag 形如 `18.10.0-tai-<commit>` |
| 拓扑 | standalone 模式：1 个 auth（SQLite 存 PVC）+ 2 个 proxy；单端口 443（multiplex） | 与本机部署形态一致 |

安装时 NOTES 会打印 “`teleportVersionOverride` MUST NOT be used…” 的警告。这是防止 chart 与二进制主版本不一致；本镜像基于同一个 18.10.0 源码，chart 也来自同一提交，可以忽略。

如果以后部署 `teleport-kube-agent`（把其他集群/节点接入），那个 chart 默认同样拉官方镜像，也必须覆盖 `image`。

## 2. 合规要点

> 以下是工程上的做法说明，不构成法律意见；有疑问请咨询法务。

- Teleport 源码以 **AGPL-3.0** 发布；Gravitational 发布的**社区版二进制/镜像**使用另一份 Community Edition 许可（有使用方规模等限制），商业版需要商业许可。
- 本方案的镜像只包含：
  - 由本仓库源码 `make full` 编译的 `teleport`、`tctl`、`tsh`、`tbot`、`fdpass-teleport`（`make image-local` 构建后与 `build/` 下文件逐字节一致）；
  - Google distroless `cc-debian12` 基础镜像；
  - Debian 软件包：`dumb-init libpam0g libaudit1 libcap-ng0 libfido2-1 libcbor0.8 libudev1 zlib1g`。
- 没有直接用 `make image`：它构建的是 v15 起已停用的 Ubuntu 重镜像（`build.assets/charts/Dockerfile`），要在 buildbox 里重新编译并打 `.deb`；官方现行的 `Dockerfile-distroless` 则是为安装发布版 `.deb` 设计的。`make image-local` 参照 distroless 的结构，但直接复制 `make full` 的产物。
- 不使用官方 helm 仓库默认镜像、不开启 `enterprise`、不开启 operator。
- 镜像带 OCI 标签：`org.opencontainers.image.licenses=AGPL-3.0-or-later`、`revision=<源码提交>`，便于追溯。
- **AGPL §13**：修改后的程序通过网络向用户提供服务时，需要向这些用户提供对应源码。请保证镜像 `revision` 对应的源码（含 TAI OIDC 改动）可以被获取。
- Web UI 使用 AGPL 版 logo（`GITHUB_REPOSITORY_OWNER` 不是 `gravitational` 时 Makefile 自动选择）。

## 3. 构建并推送镜像

```bash
cd <teleport 仓库>
make full            # 从源码编译二进制和 Web 资源（已编译且代码未变可跳过）
make image-local     # 生成 teleport-tai:<版本>-tai-<提交>
```

- `make image-local` 只使用 `build/` 下的二进制，缺文件会报错；二进制不是从当前 HEAD 编译的会打印 WARNING。
- tag 默认取二进制实际编译时的提交；可用 `LOCAL_IMAGE=... LOCAL_IMAGE_TAG=...` 覆盖。
- 当前只构建 `linux/amd64`。

验证并推送（以 `REGISTRY` 代表你的镜像仓库）：

```bash
IMG=teleport-tai:18.10.0-tai-7f2e4326ba
docker run --rm --entrypoint teleport $IMG version   # 应显示 git:...-g7f2e4326ba1
docker image inspect $IMG --format '{{json .Config.Labels}}'

docker tag $IMG REGISTRY/teleport/$IMG
docker push REGISTRY/teleport/$IMG
```

## 4. 部署

前提：

- 集群可创建 `LoadBalancer` 和 PVC；
- 已安装 **cert-manager**，并有一个能做 **DNS-01** 的 `ClusterIssuer`（见 4.1）；
- 已安装 **external-dns**，管理 `oitcep.com` 所在的 DNS zone，且 `--source` 包含 `service`、`--domain-filter`（如有）覆盖 `teleport.agpl.ap-singapore.oitcep.com`。

values 中的域名（`clusterName` 与 external-dns 注解）必须一致，当前为 `teleport.agpl.ap-singapore.oitcep.com`。

### 4.1 cert-manager Issuer

chart 申请的证书固定包含 `teleport.agpl.ap-singapore.oitcep.com` 和 `*.teleport.agpl.ap-singapore.oitcep.com`。Let's Encrypt 只能用 **DNS-01** 签发通配证书，HTTP-01 的 issuer 会一直卡在 pending。

已有可用的 DNS-01 `ClusterIssuer` 时，把它的名字填进 `values.yaml` 的 `highAvailability.certManager.issuerName` 即可。没有的话，按 DNS 服务商创建，例如（Cloudflare 示意，其他服务商替换 `solvers`；腾讯云 DNSPod 需要额外安装 cert-manager 的 DNSPod webhook）：

```yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-dns01
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    # 首次调试建议先用 staging，避免触发正式环境限流：
    # server: https://acme-staging-v02.api.letsencrypt.org/directory
    email: <运维邮箱>
    privateKeySecretRef:
      name: letsencrypt-dns01-account
    solvers:
    - dns01:
        cloudflare:
          apiTokenSecretRef:
            name: cloudflare-api-token   # 放在 cert-manager 所在命名空间
            key: api-token
```

### 4.2 external-dns

values 只在 proxy 的 Service 上加注解：

```yaml
proxy:
  annotations:
    service:
      external-dns.alpha.kubernetes.io/hostname: "teleport.agpl.ap-singapore.oitcep.com,*.teleport.agpl.ap-singapore.oitcep.com"
      external-dns.alpha.kubernetes.io/ttl: "300"
```

- 不要写在顶层 `annotations.service`：那会同时作用于 auth 的 Service，其中 `teleport-auth-v17/v18` 是 headless Service，external-dns 会把公网域名解析到 auth Pod 的内网 IP。
- LoadBalancer 拿到地址后，external-dns 创建两条 A 记录（主域名 + 通配）以及它自己的 TXT 归属记录。
- external-dns 不会覆盖不归它管的已有同名记录；如果 DNS 里手工建过 `teleport.agpl.ap-singapore.oitcep.com`，先删掉。

### 4.3 安装


```bash
kubectl create namespace teleport

# 1) 镜像仓库凭据（私有仓库时）
kubectl -n teleport create secret docker-registry teleport-registry \
  --docker-server=REGISTRY --docker-username=... --docker-password=...

```

证书由 cert-manager 生成到 secret `teleport-tls`，**不要**再手工创建同名 secret。

修改 `values.yaml`：

- `image` 与 `enterpriseImage`：改成 `REGISTRY/teleport/teleport-tai`；
- `teleportVersionOverride`：改成实际 tag；
- `imagePullSecrets`：私有仓库时填 `- name: teleport-registry`；
- `highAvailability.certManager.issuerName`/`issuerKind`：改成你的 issuer；
- `persistence.storageClassName`、`proxy.annotations.service`：按集群环境调整（如 TKE CLB 内网注解，与 external-dns 注解放在一起）。

安装：

```bash
helm upgrade --install teleport examples/chart/teleport-cluster \
  -n teleport -f tai-oidc/k8s/values.yaml --wait --timeout 10m

kubectl -n teleport get certificate        # READY 为 True，DNS-01 通常需要 1-3 分钟
kubectl -n teleport get pods,svc,pvc       # proxy 会等证书 secret 生成后才启动
kubectl -n teleport get svc teleport       # EXTERNAL-IP 即 external-dns 写入的地址
dig +short teleport.agpl.ap-singapore.oitcep.com
```

如需离线分发 chart：`helm package examples/chart/teleport-cluster` 生成 `teleport-cluster-18.10.0.tgz`，与 `values.yaml` 一起交付。

验证：

```bash
curl -sS https://teleport.agpl.ap-singapore.oitcep.com/webapi/ping -o /dev/null -w 'tls=%{ssl_verify_result} http=%{http_code}\n'
curl -s https://teleport.agpl.ap-singapore.oitcep.com/web/config.js | grep -o '"oidc":[a-z]*'   # "oidc":true
kubectl -n teleport exec deploy/teleport-auth -- tctl status
```

## 5. TAI 连接器（唯一登录方式）

### 5.1 登录策略

`values.yaml` 中：

```yaml
authentication:
  type: oidc          # 默认登录方式：OIDC
  connectorName: tai  # 默认连接器：tai（Web 登录页和 tsh login 都直接走 TAI）
  localAuth: false    # 关闭本地用户名/密码登录
```

`localAuth: false` 由 auth 服务端强制执行，不只是隐藏界面：

- 用户名/密码登录（Web 与 `tsh login --auth=local`）被拒绝，审计日志记录 `local auth disabled`；
- 邀请链接设置密码、重置密码、账号恢复同样被拒绝（`tctl users add/reset` 仍能生成链接，但链接无法完成设置）；
- Web 登录页不显示密码表单，只显示 TAI 按钮。

不受影响的：`kubectl exec … tctl`（auth Pod 内用本地管理身份，不经过登录）、proxy 加入集群的 token、`tbot` 机器身份。

因此**不需要、也不应创建本地管理员**。管理入口只有两个：TAI 登录后拥有 `editor` 的用户，以及能 `kubectl exec` 进 `teleport` 命名空间的 k8s 管理员（应急入口，请用 k8s RBAC 控制这个权限）。

### 5.2 创建连接器

k8s 集群的域名与本机部署不同，**先在 TAI 后台为应用登记新的回调地址** `https://teleport.agpl.ap-singapore.oitcep.com/v1/webapi/oidc/callback`，否则 TAI 会报 `redirect_uri` 不匹配。

安装完成到连接器创建之前，没有人能登录（`tsh`/Web 会报找不到连接器），这是预期的。连接器在本地生成（含 secret），经 stdin 传入后删除：

```bash
umask 077
tctl sso configure oidc --preset tai \
  --issuer-url https://tai.it.tencent.com/api/auth-center/oauth2/ \
  --id teleport-agpl --secret '<CLIENT_SECRET>' \
  --redirect-url https://teleport.agpl.ap-singapore.oitcep.com/v1/webapi/oidc/callback \
  -r 'user_name,lideding,editor,access,auditor' > /tmp/tai.yaml
kubectl -n teleport exec -i deploy/teleport-auth -- tctl create -f < /tmp/tai.yaml
shred -u /tmp/tai.yaml
```

- 没有本地 `tctl` 时，可以用镜像里的：`docker run --rm --entrypoint tctl REGISTRY/teleport/teleport-tai:<tag> sso configure oidc ...`。
- 连接器名必须是 `tai`，与 `connectorName` 一致。
- 授权白名单、超级管理员角色（`tai-superadmin`）、增删用户的写法见仓库根目录 `TAI_OIDC_CONFIG.md` 第 4 节；把其中的 `tctl` 换成 `kubectl -n teleport exec -i deploy/teleport-auth -- tctl` 即可。
- **不要**加 `user_name: '*'` 这类通配规则。

登录：浏览器打开 `https://teleport.agpl.ap-singapore.oitcep.com` 点 TAI；命令行 `tsh login --proxy=teleport.agpl.ap-singapore.oitcep.com`（默认即 TAI，无需 `--auth`）。

### 5.3 防止出现第二个登录入口

`connectorName` 只决定“默认”连接器。拥有 `editor` 角色的人仍可以再创建 GitHub 连接器，它会作为另一个按钮出现在登录页。请只把 `editor` 给可信管理员，并定期检查：

```bash
kubectl -n teleport exec deploy/teleport-auth -- tctl get oidc,github --format=text   # 应只有 oidc/tai
```

### 5.4 应急（TAI 不可用或连接器配错）

```bash
# 查看 / 修正连接器（不需要登录）
kubectl -n teleport exec deploy/teleport-auth -- tctl get oidc/tai
kubectl -n teleport exec -i deploy/teleport-auth -- tctl create -f < fixed-tai.yaml
```

TAI 长时间不可用而又必须登录时，临时打开本地登录，用完立即关闭并删除临时用户：

```bash
helm upgrade teleport examples/chart/teleport-cluster -n teleport -f tai-oidc/k8s/values.yaml \
  --set authentication.type=local --set authentication.localAuth=true
kubectl -n teleport exec deploy/teleport-auth -- tctl users add breakglass --roles=editor,access
# ……处理完成后
kubectl -n teleport exec deploy/teleport-auth -- tctl users rm breakglass
helm upgrade teleport examples/chart/teleport-cluster -n teleport -f tai-oidc/k8s/values.yaml
```

## 6. 运维

**证书续期**：由 cert-manager 自动完成（默认到期前 30 天）。secret 更新后 proxy 最多 12 小时内自动加载；需要立即生效时 `kubectl -n teleport rollout restart deploy/teleport-proxy`。查看状态：`kubectl -n teleport describe certificate teleport`。

**DNS**：external-dns 随 Service 地址变化自动更新记录；`helm uninstall` 删除 Service 后（`--policy=sync` 时）记录也会被删除。

**升级**：改代码 → `make full && make image-local` → 推送新 tag → 修改 `teleportVersionOverride` → `helm upgrade`。
跨 Teleport 主版本升级时，先把 fork 移植到新版本（`TAI_OIDC_IMPLEMENTATION.md`），并使用新版本仓库里的 chart。

**数据**：auth 的状态（CA、用户、连接器、审计）在 PVC `teleport` 中，删除 release 不会删除 PVC，删 PVC 等于重建集群。

**与本机部署的关系**：k8s 中是一套新集群（新的 CA 与数据），不会自动迁移本机 `/var/lib/teleport` 的数据；管理员、连接器、角色需要重新创建，已登录的 `tsh` 需要重新登录。同一域名同一时间只能指向其中一套。

## 7. 排错

| 现象 | 处理 |
|---|---|
| Pod `ImagePullBackOff` | 检查 `image`/tag、`imagePullSecrets` |
| 部署前校验 Job 失败，安装被阻断 | `kubectl -n teleport logs job/teleport-auth-test`（或 `job/teleport-proxy-test`），通常是 values 写错 |
| proxy 一直 Init | `wait-auth-update` 在等 auth 就绪，查看 auth Pod 日志 |
| proxy Pod 一直 `ContainerCreating` | 等待 secret `teleport-tls`：`kubectl -n teleport describe certificate teleport`、`kubectl get challenges -A`；HTTP-01 issuer 签不了通配证书，需换 DNS-01 |
| Certificate 卡在 `False` / challenge pending | DNS-01 凭据或 zone 不对；查看 `kubectl -n cert-manager logs deploy/cert-manager` |
| 域名不解析 | `kubectl -n teleport get svc teleport` 是否有 EXTERNAL-IP；external-dns 日志里有没有 `CREATE: teleport.agpl.ap-singapore...`；检查 `--domain-filter` 和已有同名记录 |
| 域名解析到内网 IP | external-dns 注解被放到了顶层 `annotations.service`，改到 `proxy.annotations.service` |
| 浏览器证书错误 | 域名与 `clusterName` 是否一致；staging issuer 签的证书浏览器不信任 |
| 登录时报找不到连接器 / ping 返回错误 | 连接器 `tai` 还没创建，按 5.2 创建 |
| 用户名密码登录提示 invalid credentials，审计里是 `local auth disabled` | 预期行为（`localAuth: false`） |
| TAI 回调失败 | 见 `TAI_OIDC_CONFIG.md` 第 6 节 |
| 日志 | `kubectl -n teleport logs deploy/teleport-auth`、`deploy/teleport-proxy` |