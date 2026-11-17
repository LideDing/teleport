# Teleport 自托管快速部署手册（Server + Linux 节点 + K8s + Web App）

> 版本：Teleport v18.11.1（源码自编译 OSS 版）
> 目标：一台 Linux Server 作为 Control Plane，接入其他 Linux 机器、一个 Kubernetes 集群，并代理一个 Web 应用。
> 配套文档：`SELF_HOSTED_COS_CDN.md`（COS 自建 CDN 与产物签名的完整原理和排错）。

## 0. 部署拓扑与变量

```text
                    ┌──────────────────────────────┐
                    │  teleport.example.com:443    │
                    │  Auth + Proxy + Node + App   │
                    └──────────┬───────────────────┘
                               │
          ┌────────────────────┼─────────────────────┐
          │                    │                     │
   Linux 节点 (node)     K8s 集群 (kube)       Web App (app)
   teleport agent        teleport-kube-agent   grafana:3000
```

本文使用以下变量，执行前先设置：

```bash
export TP_DOMAIN="teleport.example.com"
export TP_VERSION="18.11.1"
export COS_BASE="https://teleport-1250000000.cos.ap-guangzhou.myqcloud.com"
export ADMIN_USER="admin"
```

DNS 要求：

| 记录 | 指向 | 用途 |
| --- | --- | --- |
| `teleport.example.com` | Server 公网 IP | Web UI / tsh / 反向隧道 |
| `*.teleport.example.com` | Server 公网 IP | 每个 Web App 的访问域名 |

防火墙至少放行 TCP 443。若使用 ACME，443 必须能被公网访问。

## 1. 生成签名密钥并编译（公钥/私钥）

Teleport 的 `teleport-update` 会校验更新产物的签名。自编译版本必须使用自己的密钥，否则运行时会报：

```text
teleport-update artifact signature public key is not configured
```

### 1.1 生成密钥对

```bash
cd /root/github.com/teleport

# 生成 ECDSA P-256 密钥对：cosign.key（私钥）/ cosign.pub（公钥）
cosign generate-key-pair

# 把公钥转成 base64，供编译时注入
export KEY=$(base64 -w0 cosign.pub)
echo "$KEY" > cosign.pub.b64
```

要求：

- `cosign.key` 只保存在构建机或离线环境，绝不能上传 COS 或放进 Web 目录；
- 主密钥和备份密钥都必须配置，本地部署可以两把都用同一个公钥。

### 1.2 编译并注入公钥

```bash
cd /root/github.com/teleport

# 先构建其他二进制（teleport-update 单独构建）
make TELEPORT_UPDATE_DEV_BUILD=1 \
  build/teleport build/tctl build/tsh build/tbot build/fdpass-teleport

# 用自建公钥构建 teleport-update
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -buildvcs=false \
  -ldflags "-X main.artifactSignaturePublicKeyB64=$KEY -X main.artifactSignatureBackupPublicKeyB64=$KEY" \
  -o build/teleport-update ./tool/teleport-update

# 验证：应输出 “Teleport is not installed...”，而不是公钥未配置
./build/teleport-update status
```

arm64 机器把 `GOARCH=amd64` 改为 `arm64` 后重复上述构建。

### 1.3 打包、签名并上传 COS

```bash
cd /root/github.com/teleport
make build-archive

cd build/artifacts
TAR="teleport-v${TP_VERSION}-linux-amd64-bin.tar.gz"

sha256sum "$TAR" > "$TAR.sha256"
cosign sign-blob --yes --key /path/to/cosign.key --b64-signature "$TAR" > "$TAR.sig"

coscli cp "$TAR"        cos://<bucket>/
coscli cp "$TAR.sha256" cos://<bucket>/
coscli cp "$TAR.sig"    cos://<bucket>/
```

最终 COS 上应有三个对象：

```text
teleport-v18.11.1-linux-amd64-bin.tar.gz
teleport-v18.11.1-linux-amd64-bin.tar.gz.sha256
teleport-v18.11.1-linux-amd64-bin.tar.gz.sig
```

COS 要求：HTTPS、匿名可读。`teleport-update` 只做普通 GET，不支持 COS 签名头或预签名 URL。

验证：

```bash
curl -fsSI "$COS_BASE/teleport-v${TP_VERSION}-linux-amd64-bin.tar.gz" | head -1
curl -fsS  "$COS_BASE/teleport-v${TP_VERSION}-linux-amd64-bin.tar.gz.sha256"
```

## 2. 部署 Teleport Server

### 2.1 安装二进制

```bash
cd /root/github.com/teleport
sudo install -m 0755 build/teleport build/tctl build/tsh /usr/local/bin/
teleport version
```

### 2.2 准备 TLS 证书

三种方式选一种。

**A. 公网域名 + ACME（推荐）**

Proxy 会自动向 Let's Encrypt 申请证书，包括后续新增的 App 子域名。要求 443 端口对公网开放，且 DNS 已生效。配置中写：

```yaml
proxy_service:
  acme:
    enabled: true
    email: ops@example.com
```

**B. 自有证书（内网 / 通配符证书）**

准备覆盖 `teleport.example.com` 和 `*.teleport.example.com` 的证书：

```yaml
proxy_service:
  https_keypairs:
    - key_file: /etc/teleport/tls.key
      cert_file: /etc/teleport/tls.crt
```

**C. 测试用自签证书**

```bash
sudo mkdir -p /etc/teleport
sudo openssl req -x509 -newkey rsa:4096 -nodes -days 365 \
  -keyout /etc/teleport/tls.key \
  -out /etc/teleport/tls.crt \
  -subj "/CN=teleport.example.com" \
  -addext "subjectAltName=DNS:teleport.example.com,DNS:*.teleport.example.com"
```

客户端连接时需加 `--insecure`，仅用于测试。

> 这里生成的 TLS 密钥只用于 Proxy 的 HTTPS。Teleport 集群自身的 CA（用于签发 SSH/Kubernetes 证书）会在首次启动时自动生成到 `/var/lib/teleport`，不需要手工创建。

### 2.3 写入 /etc/teleport.yaml

以下配置已通过 `teleport configure --test` 校验。ACME 与 `https_keypairs` 二选一，不要同时启用。

```bash
sudo tee /etc/teleport.yaml > /dev/null <<'EOF'
version: v3

teleport:
  nodename: teleport.example.com
  data_dir: /var/lib/teleport
  log:
    output: stderr
    severity: INFO
    format:
      output: text

auth_service:
  enabled: true
  cluster_name: teleport.example.com
  listen_addr: 0.0.0.0:3025

proxy_service:
  enabled: true
  public_addr: teleport.example.com:443
  listen_addr: 0.0.0.0:3023
  web_listen_addr: 0.0.0.0:443
  tunnel_listen_addr: 0.0.0.0:3024
  # 方式 A：ACME
  acme:
    enabled: true
    email: ops@example.com
  # 方式 B：自有证书（与 ACME 二选一）
  # https_keypairs:
  #   - key_file: /etc/teleport/tls.key
  #     cert_file: /etc/teleport/tls.crt

ssh_service:
  enabled: true
  listen_addr: 0.0.0.0:3022
  labels:
    env: prod
    role: control-plane

app_service:
  enabled: true
  apps:
    - name: grafana
      uri: http://127.0.0.1:3000
      public_addr: grafana.teleport.example.com
      labels:
        env: prod
      # 后端是自签 HTTPS 时打开
      # insecure_skip_verify: true
      # 应用生成绝对 URL 时，把内部地址重写为公网地址
      # rewrite:
      #   redirect:
      #     - "http://127.0.0.1:3000"
EOF
```

校验：

```bash
sudo teleport configure --test=/etc/teleport.yaml
```

### 2.4 systemd 与环境变量

```bash
sudo tee /etc/systemd/system/teleport.service > /dev/null <<'EOF'
[Unit]
Description=Teleport Service
After=network.target

[Service]
Type=simple
Restart=always
RestartSec=5
EnvironmentFile=-/etc/default/teleport
ExecStart=/usr/local/bin/teleport start --config=/etc/teleport.yaml --pid-file=/run/teleport.pid
ExecReload=/bin/sh -c "exec pkill -HUP -L -F /run/teleport.pid"
PIDFile=/run/teleport.pid
LimitNOFILE=524288

[Install]
WantedBy=multi-user.target
EOF

sudo tee /etc/default/teleport > /dev/null <<EOF
TELEPORT_CDN_BASE_URL=${COS_BASE}
EOF
```

`TELEPORT_CDN_BASE_URL` 设置在 Server 上，用于 Web UI 生成安装脚本（脚本会把 COS 地址写入 Agent 的 `update.yaml`）。不要省略，否则 OSS 构建会拒绝使用官方 CDN。

客户端机器如果要启用 `tsh`/`tctl` 自动更新，也必须在客户端进程环境中设置同一个变量：

```bash
# 客户端机器的 /etc/profile.d/teleport-cdn.sh
export TELEPORT_CDN_BASE_URL=https://teleport-1250000000.cos.ap-guangzhou.myqcloud.com
```

### 2.5 启动并检查

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now teleport
sudo systemctl status teleport --no-pager
sudo journalctl -u teleport -n 100 --no-pager
```

看到 `Auth service is ready`、`Proxy service is ready` 即成功。

## 3. 创建管理员

在 Server 上执行（必须在 Auth 所在机器本地执行）：

```bash
tctl users add "$ADMIN_USER" \
  --roles=editor,access \
  --logins=root,ubuntu \
  --kubernetes-groups=system:masters
```

说明：

- `access` 是内置角色，已包含 Node、App、Kubernetes 的访问权限；
- `--logins` 是 SSH 登录用户名，按目标机器实际情况调整；
- `--kubernetes-groups=system:masters` 让该用户在 K8s 中拥有集群管理员权限，仅建议实验环境使用；生产环境请换成自定义组并在 K8s 中做 RoleBinding。

命令会输出一个注册 URL。浏览器打开该 URL，设置密码并绑定 MFA。

> 如果不需要客户端工具自动更新，可以关闭：`tctl autoupdate client-tools disable`，并在客户端设置 `TELEPORT_TOOLS_VERSION=off`。

登录验证：

```bash
tsh login --proxy=${TP_DOMAIN} --user="$ADMIN_USER"
tctl status
```

## 4. 添加 Linux 机器

### 4.1 Server 自身

配置中的 `ssh_service.enabled: true` 已把 Server 注册为节点。验证：

```bash
tsh ls
```

### 4.2 其他 Linux 机器

**第一步：在 Server 上创建 join token**

```bash
tctl tokens add --type=node --ttl=1h
# 复制输出中的 token
```

**第二步：在目标机器安装二进制**

```bash
curl -fsSLO "${COS_BASE}/teleport-v${TP_VERSION}-linux-amd64-bin.tar.gz"
tar xzf "teleport-v${TP_VERSION}-linux-amd64-bin.tar.gz"
sudo install -m 0755 teleport/teleport /usr/local/bin/teleport
```

arm64 机器把 URL 中的 `amd64` 换成 `arm64`。

**第三步：生成节点配置**

```bash
sudo teleport configure \
  --roles=node \
  --node-name="$(hostname)" \
  --auth-server="${TP_DOMAIN}:443" \
  --token="<上一步的 token>" \
  --node-labels=env=prod,role=app \
  -o file
```

该命令写入 `/etc/teleport.yaml`，内容包含：

```yaml
teleport:
  join_params:
    method: token
    token_name: "<token>"
  auth_server: "teleport.example.com:443"
```

**第四步：安装 systemd 并启动**

把 2.4 节的 unit 文件内容写入目标机器的 `/etc/systemd/system/teleport.service`（节点不需要 `TELEPORT_CDN_BASE_URL`，但保留也无害），然后启动：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now teleport
sudo journalctl -u teleport -n 50 --no-pager
```

**第五步：验证**

回到 Server 或管理员机器执行：

```bash
tctl nodes ls
tsh ls
tsh ssh root@<节点主机名或标签>
```

## 5. 添加 Kubernetes 集群

K8s 集群通过 `teleport-kube-agent` 主动连接 Proxy，不需要在 K8s 上开放入站端口。

### 5.1 创建 kube join token

在 Server 上：

```bash
tctl tokens add --type=kube --ttl=1h
# 复制 token
```

### 5.2 安装 Helm Agent

在能执行 `kubectl` / `helm` 的机器上（可以是 Server）。以下命令在 Teleport 仓库根目录执行；如果在其他机器执行，请把 `examples/chart/teleport-kube-agent` 拷过去，或使用下面的官方 Helm 仓库方式：

```bash
cat > kube-agent-values.yaml <<'EOF'
roles: kube
proxyAddr: teleport.example.com:443
authToken: "<kube token>"
kubeClusterName: prod-k8s
labels:
  env: prod
EOF

helm install teleport-kube-agent ./examples/chart/teleport-kube-agent \
  --namespace teleport \
  --create-namespace \
  -f kube-agent-values.yaml
```

如果使用官方 Helm 仓库：

```bash
helm repo add teleport https://charts.releases.teleport.dev
helm install teleport-kube-agent teleport/teleport-kube-agent \
  --version ${TP_VERSION} \
  --namespace teleport --create-namespace \
  -f kube-agent-values.yaml
```

自签证书或私有 CA 时，在 values 中加：

```yaml
# 测试：跳过 Proxy 证书校验
insecureSkipProxyTLSVerify: true

# 生产：把私有 CA 放进 Secret，再挂载信任
tls:
  existingCASecretName: teleport-ca
```

### 5.3 镜像拉取不到时

Chart 默认镜像为 `public.ecr.aws/gravitational/teleport-distroless:18.11.1`。如果网络不可达，先把镜像同步到自有仓库，然后覆盖：

```yaml
image: registry.example.com/teleport-distroless:18.11.1
imagePullSecrets:
  - name: regcred
```

也可以基于自编译二进制构建镜像。注意 `teleport` 依赖 cgo（`lib/system` 的 signal handler），不能用 `CGO_ENABLED=0`；镜像基础层必须带 glibc：

```bash
cd /root/github.com/teleport
CGO_ENABLED=1 go build -trimpath -buildvcs=false -o teleport-agent ./tool/teleport
```

```dockerfile
FROM gcr.io/distroless/base-debian12:nonroot
COPY teleport-agent /usr/local/bin/teleport
ENTRYPOINT ["/usr/local/bin/teleport", "start", "--config=/etc/teleport/teleport.yaml"]
```

### 5.4 验证

```bash
kubectl -n teleport get pods
kubectl -n teleport logs deploy/teleport-kube-agent --tail=100

tsh kube ls
tsh kube login prod-k8s
kubectl get nodes
kubectl get pods -A
```

## 6. 代理 Web App

### 6.1 静态配置（已包含在 Server 配置中）

`app_service` 已配置 Grafana 示例：

```yaml
app_service:
  enabled: true
  apps:
    - name: grafana
      uri: http://127.0.0.1:3000
      public_addr: grafana.teleport.example.com
      labels:
        env: prod
```

要点：

- `uri` 是 App Service 所在机器的视角，不是用户浏览器的视角；
- `public_addr` 必须是 `TP_DOMAIN` 的子域名，否则 Proxy 无法按 SNI 路由；
- 后端是自签 HTTPS 时加 `insecure_skip_verify: true`；
- 应用返回绝对 URL 时用 `rewrite.redirect` 把内部地址重写为公网地址。

修改后重启：

```bash
sudo systemctl restart teleport
```

### 6.2 验证

```bash
tsh apps ls
```

浏览器访问：

```text
https://grafana.teleport.example.com
```

首次访问会跳转到 Teleport 登录页，登录后由 Proxy 代理到后端。

如果 App 运行在其他机器上，建议单独部署一个 App Agent：

```bash
tctl tokens add --type=app --ttl=1h

sudo teleport configure \
  --roles=app \
  --node-name="$(hostname)" \
  --auth-server="${TP_DOMAIN}:443" \
  --token="<app token>" \
  --app-name=grafana \
  --app-uri=http://127.0.0.1:3000 \
  -o file
```

### 6.3 动态注册（可选）

如果不想把 App 写进配置文件，可以启用资源监听：

```yaml
app_service:
  enabled: true
  resources:
    - labels:
        "teleport.dev/app": "grafana"
```

然后用 `tctl create` 创建 `app` 资源：

```yaml
kind: app
version: v3
metadata:
  name: grafana
  labels:
    teleport.dev/app: grafana
spec:
  uri: http://127.0.0.1:3000
  public_addr: grafana.teleport.example.com
```

## 7. 验收清单

| 检查项 | 命令 | 预期 |
| --- | --- | --- |
| 集群状态 | `tctl status` | 显示集群名和 CA |
| 用户登录 | `tsh login --proxy=teleport.example.com` | 登录成功 |
| Linux 节点 | `tsh ls` | 看到 Server 和其他节点 |
| SSH 访问 | `tsh ssh root@<node>` | 成功登录 |
| K8s 集群 | `tsh kube ls` | 看到 `prod-k8s` |
| K8s 访问 | `tsh kube login prod-k8s && kubectl get nodes` | 返回节点列表 |
| Web App | `tsh apps ls` | 看到 `grafana` |
| App 访问 | 浏览器打开 App 域名 | 正常代理 |
| COS 安装脚本 | `curl -fsS https://teleport.example.com/scripts/install.sh \| grep base-url` | 包含 COS 地址 |

## 8. 常见问题

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| `teleport-update artifact signature public key is not configured` | updater 未注入公钥 | 按 1.2 重新编译 |
| `artifact signature verification failed for all trusted keys` | 产物签名与公钥不匹配 | 用配套私钥重新签名，或测试环境设 `insecure_skip_signature_verify: true` |
| Web UI 安装脚本报 AGPL/Community 错误 | Proxy 未设置 `TELEPORT_CDN_BASE_URL` | 按 2.4 配置并重启 |
| `tsh` 报 x509 / 证书错误 | 证书域名不匹配或自签 | 检查证书 SAN，测试时加 `--insecure` |
| ACME 申请失败 | 443 未开放或 DNS 未生效 | 检查 DNS 和安全组 443 入站 |
| K8s Agent CrashLoopBackOff | token 过期、Proxy 地址错误、CA 不受信 | `kubectl -n teleport logs`，检查 token 和 `proxyAddr` |
| `tsh kube login` 后 kubectl 403 | 用户没有 `kubernetes_groups/users` 或 K8s RBAC 未授权 | 检查 role 的 `kubernetes_groups`，或改用自定义组和 RoleBinding |
| App 502 | App Service 到后端不可达 | 在 App Service 机器上 `curl <uri>`，检查防火墙 |
| App 页面跳转到内部地址 | 后端返回绝对 URL | 配置 `rewrite.redirect` |
| 节点加入失败 | token 过期或 `auth_server` 端口错误 | 重新 `tctl tokens add`，确认使用 `teleport.example.com:443` |
| 节点上 `teleport` 启动报缺少 libpam | 二进制带 PAM 但目标机未安装 | 在节点安装 libpam，或使用不带 `pam` tag 的构建 |
| `build constraints exclude all Go files in lib/system` | 用 `CGO_ENABLED=0` 编译 `teleport` | 改用 `CGO_ENABLED=1` + `aarch64-linux-gnu-gcc`，或在 ARM64 机器上原生构建 |

## 附录 A：完整 Server 配置

见 2.3 节。修改配置后先校验：

```bash
sudo teleport configure --test=/etc/teleport.yaml
```

## 附录 B：常用运维命令

```bash
# 服务状态
sudo systemctl status teleport
sudo journalctl -u teleport -f

# 节点与资源
tctl nodes ls
tctl apps ls
tctl kube ls
tctl users ls

# 服务端与客户端版本
teleport version
tsh version
tctl version

# 备份数据目录（SQLite 默认后端）
sudo systemctl stop teleport
sudo tar czf /backup/teleport-$(date +%F).tar.gz /var/lib/teleport
sudo systemctl start teleport
```

## 附录 C：相关文档

- `SELF_HOSTED_COS_CDN.md`：COS CDN、产物命名、签名格式、`teleport-update` 签名校验细节；
- `docs/pages/upgrading/agent-managed-updates/agent-managed-updates.mdx`：Agent 自动更新；
- `docs/pages/upgrading/client-tools-managed-updates.mdx`：tsh/tctl 自动更新；
- `examples/chart/teleport-kube-agent/README.md`：K8s Agent 参数说明。


## 附录 D：ARM64 适配

Teleport 的 Server 和 Agent 是同一个 `teleport` 二进制，`tsh`/`tctl` 是客户端工具。适配 ARM64 不是"再写一个客户端"，而是为每个要运行在 ARM64 上的角色产出对应的二进制。

### D.1 角色与产物对照

| ARM64 机器角色 | 需要的产物 |
| --- | --- |
| Linux 节点（被 SSH/会话管理） | arm64 的 `teleport` |
| 用户工作站跑 `tsh` | arm64 的 `tsh` |
| 管理员跑 `tctl` | arm64 的 `tctl` |
| 启用 Agent 自动更新 | arm64 的 `teleport-update`，以及 arm64 tarball 的 `.sha256` / `.sig` |
| K8s 节点是 arm64 | arm64 或 multi-arch 的 Agent 镜像 |
| Server 本身跑在 arm64 | arm64 的 `teleport`（Auth/Proxy） |

不需要重新生成的东西：

- **签名密钥**：同一对 cosign 密钥可以签所有架构；公钥只注入一次到各架构的 `teleport-update`；
- **集群 CA**：由 Server 自动生成，与架构无关；
- **CDN 地址与版本号**：仍使用同一个 `TELEPORT_CDN_BASE_URL` 和 `18.11.1`；
- **每个 tarball 单独生成** `.sha256` / `.sig`，签名命令不变。

### D.2 交叉编译 ARM64 二进制

`teleport`、`tsh`、`tctl` 依赖 cgo（`lib/system`、`lib/backend/lite`、`session/shell`），**不能用 `CGO_ENABLED=0` 交叉编译**。推荐两种方式。

**方式 1（推荐）：在 ARM64 机器上原生构建**

```bash
cd /root/github.com/teleport

make TELEPORT_UPDATE_DEV_BUILD=1 full

# 用自建公钥重编 teleport-update
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false \
  -ldflags "-X main.artifactSignaturePublicKeyB64=$KEY -X main.artifactSignatureBackupPublicKeyB64=$KEY" \
  -o build/teleport-update ./tool/teleport-update
```

**方式 2：在 amd64 上交叉编译**

安装 aarch64 交叉工具链（Ubuntu/Debian）：

```bash
sudo apt install gcc-aarch64-linux-gnu
```

然后构建：

```bash
cd /root/github.com/teleport

for t in teleport tsh tctl; do
  CC=aarch64-linux-gnu-gcc CGO_ENABLED=1 GOOS=linux GOARCH=arm64 \
    go build -trimpath -buildvcs=false -o build-arm64/$t ./tool/$t
done

# tbot 和 teleport-update 是纯 Go，可以关闭 cgo
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false \
  -o build-arm64/tbot ./tool/tbot
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false \
  -ldflags "-X main.artifactSignaturePublicKeyB64=$KEY -X main.artifactSignatureBackupPublicKeyB64=$KEY" \
  -o build-arm64/teleport-update ./tool/teleport-update
```

> 交叉编译 `teleport`/`tsh`/`tctl` 需要 ARM64 的 C 库；如果开启 PAM/FIPS，还需要对应的 ARM64 库。最省事的是在 ARM64 机器上原生构建，或使用 Teleport 的 buildbox。
>
> `fdpass-teleport` 是 Rust 二进制，交叉编译需要额外的 Rust target。只做 SSH 节点时可以不打进 tarball；需要完整标准 tarball 时，建议在 ARM64 机器上构建。

### D.3 打包、签名与上传

**方式 1（ARM64 原生构建）**：二进制在 `build/` 下，直接打包：

```bash
cd /root/github.com/teleport
make build-archive
cd build/artifacts
TAR="teleport-v${TP_VERSION}-linux-arm64-bin.tar.gz"
```

**方式 2（交叉编译）**：二进制在 `build-arm64/` 下：

```bash
cd /root/github.com/teleport
make ARCH=arm64 BUILDDIR=build-arm64 \
  BINARIES="build-arm64/teleport build-arm64/tsh build-arm64/tctl build-arm64/tbot build-arm64/teleport-update" \
  build-archive
cd build-arm64/artifacts
TAR="teleport-v${TP_VERSION}-linux-arm64-bin.tar.gz"
```

两种方式最后都执行：

```bash
sha256sum "$TAR" > "$TAR.sha256"
cosign sign-blob --yes --key /path/to/cosign.key --b64-signature "$TAR" > "$TAR.sig"

coscli cp "$TAR"        cos://<bucket>/
coscli cp "$TAR.sha256" cos://<bucket>/
coscli cp "$TAR.sig"    cos://<bucket>/
```

COS 上最终会有：

```text
teleport-v18.11.1-linux-amd64-bin.tar.gz
teleport-v18.11.1-linux-amd64-bin.tar.gz.sha256
teleport-v18.11.1-linux-amd64-bin.tar.gz.sig
teleport-v18.11.1-linux-arm64-bin.tar.gz
teleport-v18.11.1-linux-arm64-bin.tar.gz.sha256
teleport-v18.11.1-linux-arm64-bin.tar.gz.sig
```

Agent 下载 URL 中的架构来自它自身的 `runtime.GOARCH`，所以 arm64 节点会自动请求 `linux-arm64` 的 tarball；amd64 节点不受影响。

### D.4 K8s 集群是 ARM64

先确认节点架构：

```bash
kubectl get nodes -o wide
```

如果节点是 `arm64`：

- 官方 `teleport-distroless` 镜像如果是 multi-arch manifest，直接使用即可；
- 如果使用自建镜像，必须构建 arm64 镜像，或构建 multi-arch manifest：

```bash
# amd64 交叉编译需要 gcc-aarch64-linux-gnu；在 ARM64 机器上直接去掉 CC 即可
CC=aarch64-linux-gnu-gcc CGO_ENABLED=1 GOOS=linux GOARCH=arm64 \
  go build -trimpath -buildvcs=false -o teleport-agent ./tool/teleport

docker buildx build --platform linux/arm64 \
  -t registry.example.com/teleport-distroless:18.11.1-arm64 --push .
```

镜像基础层使用 `gcr.io/distroless/base-debian12`（带 glibc），不要用 `static`。

然后在 Helm values 中把 `image` 指向该镜像。

### D.5 macOS ARM64 客户端

macOS 的 CDN 产物不是 Linux tarball，而是 `.pkg`：

- 客户端工具（18.1.5+）：`teleport-tools-18.11.1.pkg`；
- Agent：`teleport-18.11.1.pkg`。

Linux ARM64 客户端直接使用 `teleport-v18.11.1-linux-arm64-bin.tar.gz` 即可；macOS 的 `.pkg` 需要单独的打包和签名流程，若只是少量 Apple Silicon 客户端，建议手动安装或使用官方包，不必为此改 CDN。

