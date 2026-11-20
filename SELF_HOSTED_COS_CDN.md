# 自编译 Teleport + 腾讯云 COS 自建 CDN 部署手册

> 本文是两个部署场景共用的参考：下载地址与签名机制、编译和签名、用 COS 分发。按场景的部署指南见 `deploy/README.md`：K8s 部署在 `deploy/k8s/`（使用 COS 分发），单机部署在 `deploy/single-host/`。
> 适用版本：Teleport v18.11.1，Linux amd64/arm64，OSS（社区源码）自编译部署。
> 目标：Proxy、Agent、客户端工具（tsh/tctl）全部从腾讯云 COS 下载 Teleport 二进制，不再访问 `cdn.teleport.dev`。

## 1. 背景

Teleport 的二进制下载地址不是 `teleport.yaml` 中的服务端配置项，而是由三个独立的使用方各自决定的：

| 使用方 | 配置方式 | 代码位置 |
| --- | --- | --- |
| Proxy 生成 Web 安装脚本 | 进程环境变量 `TELEPORT_CDN_BASE_URL` | `lib/web/scripts.go:145-170` |
| Agent 的 `teleport-update` | `update.yaml` 的 `spec.base_url`，或 `teleport-update enable --base-url` | `lib/autoupdate/agent/config.go:81-82`、`lib/autoupdate/agent/updater.go:1033-1039` |
| 客户端 `tsh` / `tctl` 自动更新 | 客户端进程环境变量 `TELEPORT_CDN_BASE_URL` | `lib/autoupdate/tools/helper.go:47-55`、`lib/autoupdate/tools/utils.go:205-215` |

本地 `make full` 默认生成 `BuildOSS` 版本（`lib/modules/modules.go:387`）。OSS 构建下：

- Proxy 的 `/scripts/install.sh` 如果没有 `TELEPORT_CDN_BASE_URL`，会直接报 AGPL/Community license 错误（`lib/web/scripts.go:163-167`）；
- Agent 更新如果没有设置 base_url，会报 `--base-url flag must be specified for AGPL edition of Teleport`（`lib/autoupdate/agent/updater.go:1033-1039`）；
- `tsh` / `tctl` 自动更新会被静默禁用（`lib/autoupdate/tools/utils.go:209-215`）。

> **重要：换 CDN 只解决“从哪里下载”，不解决“签名校验”。**
> v18.11.1 的 `teleport-update` 会校验产物的分离签名（`lib/autoupdate/agent/installer.go:234-236`）。自编译的 updater 如果没有内置公钥，连 `status` 都跑不起来，会报 `teleport-update artifact signature public key is not configured`（`lib/autoupdate/agent/installer.go:362-368`）。因此本手册同时包含自建密钥与签名步骤。

## 2. 前置条件

- 已检出 Teleport v18.11.1 源码；
- Go 1.25+、make、gcc 等 `make full` 所需工具链；
- 已安装 OpenSSL（用于生成密钥和签名，系统一般自带）；
- 一个腾讯云 COS 桶，满足：
  - 使用 HTTPS 访问（`spec.base_url` 只接受 `https://`，见 `lib/autoupdate/agent/config.go:242-245`）；
  - 允许匿名 GET（`teleport-update` 使用普通 HTTP GET，不支持 COS 签名头或预签名 URL）；
  - 建议在 COS 前挂腾讯云 CDN，绑定自定义域名并开启 HTTPS。

## 3. 生成签名密钥

只需执行一次。私钥必须离线妥善保管，公钥会编译进 `teleport-update`。

```bash
cd /root/github.com/teleport

# 生成 ECDSA P-256 私钥；需要密码时在末尾加 -aes256
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:prime256v1 \
  -out teleport-signing.key
chmod 600 teleport-signing.key

# 导出公钥并转成 base64，供编译时注入
openssl pkey -in teleport-signing.key -pubout -out teleport-signing.pub
export KEY=$(base64 -w0 teleport-signing.pub)
echo "$KEY" > teleport-signing.pub.b64
```

说明：

- 主密钥和备份密钥必须同时配置；本地部署可以两个都用同一把密钥。
- 编译进二进制的公钥格式为：PEM（`PUBLIC KEY`）→ base64（标准编码）→ 通过 ldflags 注入。
- 签名算法为 ECDSA P-256 + SHA-256，签名文件内容是 ASN.1 DER 签名的 base64 编码。
- 私钥可用 `-aes256` 加密；`sign-teleport-artifacts.sh` 通过 `TELEPORT_SIGN_PASS` 读取密码。

## 4. 编译

### 4.1 构建除 teleport-update 外的二进制

```bash
cd /root/github.com/teleport

make TELEPORT_UPDATE_DEV_BUILD=1 \
  build/teleport build/tctl build/tsh build/tbot build/fdpass-teleport
```

### 4.2 用自建公钥构建 teleport-update

不能使用 `TELEPORT_UPDATE_DEV_BUILD=1` 的产物：它没有内置公钥，运行时所有命令都会失败。必须显式注入公钥。

> 注意：Makefile 里的 `TELEPORT_UPDATE_ARTIFACT_SIGNATURE_PUBLIC_KEY_B64` 注入块只在
> `GITHUB_REPOSITORY_OWNER=gravitational` 时生效（`Makefile:405`），本地直接 `make` 会静默产出空公钥的 updater，
> 表现为 `teleport-update artifact signature public key is not configured`。请用下面的 go build：

```bash
cd /root/github.com/teleport

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -buildvcs=false \
  -ldflags "-X main.artifactSignaturePublicKeyB64=$KEY -X main.artifactSignatureBackupPublicKeyB64=$KEY" \
  -o build/teleport-update ./tool/teleport-update
```

验证 updater 可以正常初始化（不应再出现公钥未配置的错误）：

```bash
./build/teleport-update status
# 预期输出：Teleport is not installed by teleport-update with this suffix.
```

arm64 机器请将 `GOARCH=amd64` 改为 `GOARCH=arm64`，并重复 4.1、4.2。

### 4.3 打包为 CDN 约定的文件名

```bash
cd /root/github.com/teleport
make build-archive
```

产物：

```text
build/artifacts/teleport-v18.11.1-linux-amd64-bin.tar.gz
```

压缩包内顶层目录必须是 `teleport/`，且包含 `teleport/teleport-update`、`teleport/VERSION`、`teleport/examples/systemd/teleport.service` 等文件（`make build-archive` 会自动按此布局打包，提取逻辑见 `lib/autoupdate/agent/installer.go:565-584`）。

文件名必须严格匹配以下规则（`lib/autoupdate/package_url.go:48-58`）：

```text
teleport-v{版本}-{os}-{arch}{-fips}-bin.tar.gz
```

例如：

```text
teleport-v18.11.1-linux-amd64-bin.tar.gz
teleport-v18.11.1-linux-arm64-bin.tar.gz
teleport-v18.11.1-linux-amd64-fips-bin.tar.gz
```

> 版本号带前缀 `v`；架构使用 Go 的 `GOARCH`（`amd64`、`arm64`、`arm`、`386`）。

## 5. 生成校验和与签名

```bash
cd /root/github.com/teleport/build/artifacts

TAR=teleport-v18.11.1-linux-amd64-bin.tar.gz

# 校验和：前 64 字节必须是十六进制 SHA-256
sha256sum "$TAR" > "$TAR.sha256"

# 分离签名：单行 base64 的 ECDSA-SHA256（ASN.1 DER）
openssl dgst -sha256 -sign /root/github.com/teleport/teleport-signing.key "$TAR" \
  | openssl base64 -A > "$TAR.sig"

# 自检：必须能被公钥验证通过
base64 -d "$TAR.sig" > /tmp/tar.sig.der
openssl dgst -sha256 -verify /root/github.com/teleport/teleport-signing.pub \
  -signature /tmp/tar.sig.der "$TAR"
```

最终每个版本需要三个对象：

```text
teleport-v18.11.1-linux-amd64-bin.tar.gz
teleport-v18.11.1-linux-amd64-bin.tar.gz.sha256
teleport-v18.11.1-linux-amd64-bin.tar.gz.sig
```

说明：

- `.sha256` 文件内容可以是 `sha256sum` 的完整输出（`<hash>  <filename>`），代码只读取前 64 个字符；
- `.sig` 是 `base64(ecdsa.SignASN1(privateKey, sha256(tarball)))`，必须是单行、无换行（`lib/autoupdate/agent/installer.go:449` 用 `base64.StdEncoding` 严格解码），所以签名时必须用 `openssl base64 -A`；
- 不开启签名校验时可以不传 `.sig`，但不建议在生产环境这样做。

批量签名时使用仓库根目录的 `sign-teleport-artifacts.sh`（换密钥后必须用新公钥重新执行 4.2 编译 `teleport-update`）：

```bash
./sign-teleport-artifacts.sh --keygen                 # 生成密钥对并签名全部产物
TELEPORT_SIGN_PASS=xxx ./sign-teleport-artifacts.sh   # 用加密私钥重新签名
```

脚本会对每个 `teleport-v*-bin.tar.gz` 生成 `.sha256` 和单行 `.sig`，并立即用公钥自检。

## 6. 上传到 COS

将三个文件上传到桶根目录（或自定义域名对应的路径前缀下）：

```bash
coscli cp teleport-v18.11.1-linux-amd64-bin.tar.gz        cos://<bucket>/
coscli cp teleport-v18.11.1-linux-amd64-bin.tar.gz.sha256 cos://<bucket>/
coscli cp teleport-v18.11.1-linux-amd64-bin.tar.gz.sig    cos://<bucket>/
```

假设最终访问地址为：

```text
https://teleport-1250000000.cos.ap-guangzhou.myqcloud.com
```

检查可访问性：

```bash
BASE=https://teleport-1250000000.cos.ap-guangzhou.myqcloud.com
curl -fsSI "$BASE/teleport-v18.11.1-linux-amd64-bin.tar.gz" | head -1
curl -fsS  "$BASE/teleport-v18.11.1-linux-amd64-bin.tar.gz.sha256"
```

预期返回 `200`，且校验和与本地一致：

```bash
sha256sum teleport-v18.11.1-linux-amd64-bin.tar.gz
```

注意事项：

- 桶不能是私有读，否则 `teleport-update` 会收到 403；
- 不要使用仅浏览器可用的预签名 URL，`teleport-update` 不会带 COS 认证参数；
- 不要在 base URL 中使用 HTTP，配置校验会直接拒绝。

## 7. 配置 Proxy

单机部署：默认 systemd 单元会加载 `/etc/default/teleport`（`examples/systemd/teleport.service`）。

```bash
sudo tee -a /etc/default/teleport > /dev/null <<'EOF'
TELEPORT_CDN_BASE_URL=https://teleport-1250000000.cos.ap-guangzhou.myqcloud.com
EOF

sudo systemctl restart teleport
```

如果使用自定义 systemd 单元，请在 `[Service]` 段加入：

```ini
Environment=TELEPORT_CDN_BASE_URL=https://teleport-1250000000.cos.ap-guangzhou.myqcloud.com
```

作用：

- Web UI 生成的安装脚本会把 `--base-url` 传给 `teleport-update enable`（`lib/web/scripts/install.go:121-141`）；
- 新安装的 Agent 会自动把该地址写入 `update.yaml`；
- 不设置时，OSS 构建的安装脚本会直接返回错误。

> 该变量是进程级环境变量，修改后必须重启 Proxy；已生成的旧脚本需要从 Web UI 重新生成。

K8s 部署：不涉及 systemd，在 `deploy/k8s/values.yaml` 的 `extraEnv` 里设置 `TELEPORT_CDN_BASE_URL`，然后在 `deploy/k8s` 下执行 `./deploy.sh install`。

## 8. 配置 Agent

### 8.1 新 Agent

直接使用 Web UI 的安装脚本或 `/scripts/install.sh`，会自动带上 Proxy 配置的 base URL。

### 8.2 已安装的 Agent

重新执行 `enable`（可安全重复执行，未显式覆盖的配置会保留）：

```bash
sudo teleport-update enable \
  --base-url https://teleport-1250000000.cos.ap-guangzhou.myqcloud.com
```

也可以直接编辑配置文件：

```yaml
# /opt/teleport/default/update.yaml
spec:
  base_url: https://teleport-1250000000.cos.ap-guangzhou.myqcloud.com
```

### 8.3 集群侧资源

自托管集群需要显式配置 `autoupdate_config` 和 `autoupdate_version`，否则不会触发 Agent 更新。版本填 `18.11.1`（不要带 `v`）：

```yaml
kind: autoupdate_version
metadata:
  name: autoupdate-version
spec:
  agents:
    start_version: 18.11.1
    target_version: 18.11.1
    schedule: regular
    mode: enabled
```

```yaml
kind: autoupdate_config
metadata:
  name: autoupdate-config
spec:
  agents:
    mode: enabled
    strategy: halt-on-error
    schedules:
      regular:
        - name: default
          days: ["Mon", "Tue", "Wed", "Thu"]
          start_hour: 16
```

应用：

```bash
tctl create -f autoupdate_version.yaml
tctl create -f autoupdate_config.yaml
```

> base URL 不会由集群下发，必须保存在每台 Agent 的 `update.yaml` 中。

## 9. 配置客户端 tsh/tctl

在客户端机器的登录环境或系统环境中设置：

```bash
# /etc/profile.d/teleport-cdn.sh
export TELEPORT_CDN_BASE_URL=https://teleport-1250000000.cos.ap-guangzhou.myqcloud.com
```

Linux 上 `tsh` / `tctl` 自动更新下载的是完整 tarball，只校验 `.sha256`，不校验签名（`lib/autoupdate/tools/updater.go:360-380`），因此同一份 COS 产物即可满足。

如果不需要客户端自动更新，关闭即可：

```bash
tctl autoupdate client-tools disable
```

或在用户侧设置：

```bash
export TELEPORT_TOOLS_VERSION=off
```

## 10. 验证

### 10.1 Agent 更新

```bash
sudo teleport-update update --now --debug
sudo journalctl -u teleport-update.service -n 100 --no-pager
```

日志中应能看到从 COS 地址下载：

```text
Downloading Teleport tarball. url=https://teleport-1250000000.cos.ap-guangzhou.myqcloud.com/teleport-v18.11.1-linux-amd64-bin.tar.gz
```

### 10.2 安装脚本

```bash
curl -fsS https://<proxy>/scripts/install.sh | grep -n 'base-url'
```

应能看到 `--base-url https://teleport-...`。

### 10.3 客户端工具

```bash
TELEPORT_CDN_BASE_URL=https://teleport-1250000000.cos.ap-guangzhou.myqcloud.com \
TELEPORT_TOOLS_VERSION=18.11.1 tsh version
```

## 11. 故障排查

| 报错 | 原因 | 处理 |
| --- | --- | --- |
| `teleport-update artifact signature public key is not configured` | updater 编译时未注入公钥 | 按 4.2 重新编译 |
| `artifact signature verification failed for all trusted keys` | 公钥与签名不匹配，或产物未签名 | 用配套私钥重新签名；测试环境可设 `insecure_skip_signature_verify: true` |
| `signature not found: ...sig` | 未上传 `.sig` | 上传签名文件，或关闭签名校验 |
| `checksum not found: ...sha256` | 未上传 `.sha256` | 上传校验和文件 |
| `Teleport download not found` | 文件名或路径不匹配 | 按第 4.3 节规则命名，确认对象在 base URL 根路径下 |
| HTTP 403 | COS 桶为私有读 | 开启公有读，或通过腾讯云 CDN 提供匿名访问 |
| `Teleport download base URL ... must use TLS` | base URL 使用了 http | 改为 https |
| `--base-url flag must be specified for AGPL edition of Teleport` | `update.yaml` 缺少 `base_url` | 重新执行 `enable --base-url` |
| 下载成功但解压失败 | tarball 顶层目录不是 `teleport/` | 使用 `make build-archive` 打包，不要手工压缩任意目录 |

## 12. 简化方案：不使用 Managed Updates

如果不需要 Agent 自动更新，可以完全不启用 `autoupdate_config`，用 tarball + systemd 手动部署和升级。此时可以不构建 `teleport-update`：

```bash
make TELEPORT_UPDATE_DEV_BUILD=1 \
  BINARIES="build/teleport build/tctl build/tsh build/tbot build/fdpass-teleport" \
  full build-archive
```

只需要处理 Proxy 的安装脚本和 `tsh` / `tctl` 的自动更新（或按第 9 节关闭）。

## 13. 附录：代码位置索引

| 功能 | 文件 |
| --- | --- |
| 默认 CDN 与 URL 模板 | `lib/autoupdate/package_url.go:42-58` |
| Proxy 安装脚本 CDN 选择与 OSS 限制 | `lib/web/scripts.go:145-170` |
| 安装脚本传递 `--base-url` | `lib/web/scripts/install.go:121-141` |
| Agent `update.yaml` 的 `base_url` | `lib/autoupdate/agent/config.go:81-82` |
| AGPL 版本强制要求 base URL | `lib/autoupdate/agent/updater.go:1033-1039` |
| 产物签名校验 | `lib/autoupdate/agent/installer.go:234-236`、`:362-368`、`:406-485` |
| 客户端工具 CDN 与 OSS 限制 | `lib/autoupdate/tools/helper.go:47-55`、`lib/autoupdate/tools/utils.go:205-215` |
| 默认安装路径 | `lib/autoupdate/agent/setup.go:52-59` |
| systemd 单元环境文件 | `examples/systemd/teleport.service` |
