# K8s 部署踩坑记录（TKE + COS，v18.11.1）

> 环境：TKE 集群 `cls-g8ta0gnm`（ap-singapore）、官方 chart `teleport-cluster 18.11.1`、
> 自编译 OSS 镜像 `v18.11.1-agpl`、腾讯云 COS 作为二进制分发源、cert-manager + DNSPod 签发证书。
>
> 本文属于 K8s 部署场景。单机部署是另一个独立场景，见 `deploy/single-host/README.md`。
>
> 配套文件：
> - `deploy/k8s/` — Helm values、部署脚本、RBAC、autoupdate 资源
> - `deploy/k8s/nodes/` — 节点批量加入脚本（SSH 批量 / 单台机器安装脚本）
>
> 本文只记录**踩过的坑**和结论。部署步骤见 `deploy/k8s/README.md`，编译、签名与 COS 分发见 `SELF_HOSTED_COS_CDN.md`。

---

## 0. 最终形态速查

| 项 | 值 |
| --- | --- |
| chart / 版本 | `teleport/teleport-cluster` **18.11.1**（固定版本） |
| 镜像 | `oitcep-tcr.tencentcloudcr.com/oitcep/teleport:v18.11.1-agpl` |
| 镜像 tag 注入 | `teleportVersionOverride: v18.11.1-agpl` |
| 对外域名 | `teleport-agpl.ap-singapore.oitcep.com` |
| 分发地址 | `https://teleport-agpl.cos.oitcep.com` |
| listener 模式 | `proxyListenerMode: separate`（chart 默认） |
| 证书 | cert-manager + ClusterIssuer `letsencrypt-dns`（DNS-01） |
| Service 注解 | `specify-protocol`（443 强制 TCP）、`direct-access`（CLB 直连 Pod） |
| 存储 | PVC 20Gi，StorageClass `cbs` |

---

## 1. 二进制与签名

### 1.1 teleport-update 没内嵌公钥（致命）

**现象**：任何 updater 命令都失败：

```text
teleport-update artifact signature public key is not configured
```

**原因**：Makefile 的公钥注入块只在 `GITHUB_REPOSITORY_OWNER=gravitational` 时生效；本地
`make` 或 `TELEPORT_UPDATE_DEV_BUILD=1` 会静默产出**空公钥**的 updater。

**处理**：显式注入公钥重编，只重编这一个二进制：

```bash
KEY=$(cat teleport-signing.pub.b64)
CGO_ENABLED=0 go build -trimpath -buildvcs=false \
  -ldflags "-w -s -X main.artifactSignaturePublicKeyB64=$KEY -X main.artifactSignatureBackupPublicKeyB64=$KEY" \
  -o build/teleport-update ./tool/teleport-update
./build/teleport-update status      # 不再报公钥未配置
```

> 其余二进制（teleport/tctl/tsh/tbot）与签名无关，不需要重编；CDN 地址是运行时环境变量。

### 1.2 签名文件必须是单行 base64

updater 用 `base64.StdEncoding` 严格解码，带换行会直接失败：

```bash
openssl dgst -sha256 -sign key.pem tarball | openssl base64 -A > tarball.sig
```

### 1.3 换二进制后必须重打包 + 重签名

替换 `teleport/teleport-update` 后，原 `.sha256`/`.sig` 必然失效；`-centos7` 那份如果只是
复制文件、签名却是复制前生成的，校验会直接失败（我们踩过）。

### 1.4 glibc 基线（已知限制）

自编译的 `teleport` 依赖 **GLIBC_2.38**（cgo `__isoc23_strtol/sscanf`），在 Debian 12 /
Ubuntu 22.04 上会报 `GLIBC_2.38 not found`；`tsh` 要 2.35。官方版用 centos7 buildbox 编译只要 2.17。
**agent 机器如果是老发行版，需要用 buildbox 重编**；`teleport-update` 是静态的，不受影响。

---

## 2. 镜像与 Chart

### 2.1 镜像 tag 与 chart 版本不一致

chart 用 `.Chart.Version` 拼 `image:tag`，我们的 tag 是 `v18.11.1-agpl`，所以必须设
`teleportVersionOverride`，helm 会打印一条 DANGER 警告（可忽略）。
更干净的做法是把镜像 retag 成 `18.11.1` 再推送，就不用 override。

### 2.2 私有仓库要拉取凭据

`oitcep-tcr.tencentcloudcr.com` 是私有实例，必须：

```yaml
imagePullSecrets:
  - name: teleport-registry
```

`deploy.sh` 会从本机 `~/.docker/config.json` 里只取该 registry 的凭据生成 Secret，不会把其他仓库的凭据带进集群。

### 2.3 clusterName 是持久化的，改域名必须重建数据卷

集群名写进 auth 后端，改 `clusterName` 后 helm upgrade 不会改掉它（`/webapi/ping` 里仍是旧名）。
变更域名 = `helm uninstall` + 删 PVC + 重新安装。**全新集群请在第一次安装前就定好域名。**

---

## 3. 腾讯云 CLB / DNS（坑最多）

### 3.1 443 被建成七层监听器 → 502

**现象**：外部访问 443 返回腾讯云 CLB 的 `{"code":0,"status":"502 Bad Gateway"}` 页面。

**原因**：TKE 对 443 端口默认按七层处理，而 Teleport 要自己终止 TLS（四层）。

**修复**：

```yaml
proxy:
  annotations:
    service:
      service.cloud.tencent.com/specify-protocol: '{"443":{"protocol":["TCP"]}}'
```

> 注意：这个注解只在**创建时**生效，改完要删掉 Service 让 TKE 重建 CLB。

### 3.2 CLB 走 nodePort 丢包 ~40%（最坑的一个）

**现象**：`teleport-update enable` 报

```text
Get "https://<proxy>:443/webapi/find?...": context deadline exceeded (Client.Timeout exceeded while awaiting headers)
```

但同一台机器 `nc` 测 443 是通的，COS 下载也正常。

**实测数据**（同一节点、同一时刻）：

| 访问方式 | 成功率 |
| --- | --- |
| 节点 → 本机 nodePort | 10/10 |
| 节点 → CLB VIP | 12/20 |
| 本机 → CLB VIP | 13/20 |

即 CLB 经 nodePort/kube-proxy 这条链路在丢 SYN，健康检查却全是 Alive。

**修复**：让 CLB 直连 Pod IP，绕开 nodePort：

```yaml
service.cloud.tencent.com/direct-access: "true"
```

修复后集群内 20/20、本机 20/20。**这个注解必须保留**，否则节点安装会随机失败。

### 3.3 externalTrafficPolicy: Local 的坑

`Local` + 单副本时，只有运行 Pod 的节点能通过健康检查，CLB 后端只剩 1 个（会告警
`Only one node in the backend`）；`Cluster` 则所有节点都可转发。
配合 `direct-access` 时该字段基本不起作用，但保持 `Cluster` 更稳。

### 3.4 external-dns 只认注解里的 hostname

external-dns 不会"自动"给整个域做泛解析，它只创建 Service/Ingress 注解中列出的名字。
之前只写了主域名，所以 **没有 `*.teleport-agpl.ap-singapore` 记录** —— 证书里有泛域名、DNS 里没有，
App Access 子域会 NXDOMAIN。

**修复**：注解里逗号分隔写两个：

```yaml
external-dns.alpha.kubernetes.io/hostname: "teleport-agpl.ap-singapore.oitcep.com,*.teleport-agpl.ap-singapore.oitcep.com"
```

### 3.5 external-dns 会卡死，换 CLB 后记录不更新

**现象**：Service 换了新 CLB，但 DNSPod 的 CNAME 还指着已删除的旧 CLB；external-dns 日志停在
17 小时前的分页报错：

```text
Code=ResourceNotFound.NoDataOfRecord, Message=记录列表为空
```

**处理**：`kubectl -n external-dns rollout restart deploy/external-dns`。若仍不更新，只能手工改记录。
**换 CLB 后务必检查主域名和泛域名两条记录。**

### 3.6 DNSPod 不接受 `cname-*` 形式的 TXT

external-dns 给每条记录写归属 TXT（`cname-<name>`），遇到泛域名时生成
`cname-*.teleport-agpl...`，DNSPod 直接拒绝：

```text
Code=InvalidParameter.SubdomainInvalid, Message=主机记录不正确
```

external-dns 会重试并最终用合法名字建出 TXT + CNAME，日志里残留的这条 error 可以忽略。
（如果换成别的 provider，注意先确认它是否允许这种命名。）

### 3.7 CLB 是 CNAME 不影响证书

证书签发走 DNS-01，Let's Encrypt 只查 DNS TXT，**不会回连你的域名**，所以 CNAME 到 CLB
甚至暂时解析不到都没关系。之前"因为 CNAME 导致证书申请失败"的真正原因是走了 HTTP-01（见 4.1）。

---

## 4. 证书

### 4.1 泛域名只能用 DNS-01

Let's Encrypt 不支持 HTTP-01 签 `*.example.com`。chart 默认就会把 `*.clusterName` 加进
Certificate 的 dnsNames，所以 **issuer 必须是 DNS-01 的**：

```yaml
acme: false
highAvailability:
  certManager:
    enabled: true
    issuerKind: ClusterIssuer
    issuerName: letsencrypt-dns   # dns01 + dnspod webhook
```

### 4.2 集群里那个 `letsencrypt-http` 不能用

它是 `http01 + nginx ingress`：既签不了泛域名，也会因为请求要打到 proxy 上而失败。
排查时先看 `kubectl get clusterissuer -o yaml | grep -A5 solvers`。

### 4.3 证书续期是自动的

cert-manager 会在到期前 30 天续；proxy 侧 `https_keypairs_reload_interval: 1h` 会自动加载新证书，
不需要重启 Pod。（helm 会打印 `privateKey.rotationPolicy` 的 warning，无害。）

---

## 5. Agent 安装 / 托管更新

### 5.1 `autoupdate_config` 不会立刻生效

只创建 `autoupdate_config` + `autoupdate_version` 时，auth 里的 rollout 控制器同步周期很长，
`autoupdate-agent-rollout` 单例迟迟不生成，`/scripts/install.sh` 会退回**旧版脚本**（不带 `--base-url`）。

**处理**：创建后重启 auth：

```bash
kubectl -n teleport rollout restart deploy/teleport-auth
kubectl -n teleport exec deploy/teleport-auth -- tctl get autoupdate_agent_rollout
```

`deploy.sh autoupdate` 已经内置了这一步。

> 旧版脚本会无视 `TELEPORT_CDN_BASE_URL`，直接从 `cdn.teleport.dev` 或官方 apt/yum 源装**官方二进制**。
> 提交 `6f6cf8ce532` 起，AGPL 构建即使没有 rollout 也只返回 teleport-update 版脚本（`lib/web/scripts.go`），
> 不再回退；之前构建的镜像（如 TKE 上的 `v18.11.1-agpl`）仍需按上面的方法处理。

### 5.2 `tctl tokens add --labels` 不会进安装脚本

它只设置 token 的 `metadata.labels`；安装脚本读的是 **`spec.suggested_labels`**。
要让 20 台机器一键带标签，必须用 YAML 建 token：

```yaml
kind: token
version: v2
metadata:
  name: <32位hex>
  expires: 2026-11-18T10:00:00Z
spec:
  roles: [Node]
  join_method: token
  suggested_labels:
    team: [team_y]
    gpu: [h200]
    env: [prod]
```

然后：

```bash
curl -fsSL 'https://<proxy>/scripts/<token>/install-node.sh' | sudo bash
```

### 5.3 安装脚本对 `/webapi/find` 只有 5 秒超时

CLB 抖动（见 3.2）会让它直接失败。修好 `direct-access` 之后才算稳定。

### 5.4 separate vs multiplex：443 与 3024

chart 默认 `proxyListenerMode: separate`：443 只跑 Web/客户端，反向隧道在 3024。

- `separate`：节点用 `auth_server=<域名>:443` 或 `:3024` 都能加入（实测）。
- 曾试过 `multiplex`：节点走 TLS routing，SNI 变成
  `<hex(域名)>.teleport.cluster.local`，代理返回公网证书 → 校验失败：

  ```text
  x509: certificate is valid for *.teleport-agpl..., teleport-agpl..., not 74656c65706f72742d....teleport.cluster.local
  ```

  所以**不要随手切 multiplex**，除非同时准备好内部证书。最终保持 `separate`。

### 5.5 改标签不要重装

节点标签由 agent 上报，`tctl` 没有服务端改标签的命令。正确做法是重新生成配置并重启：

```bash
C=/etc/teleport.yaml
/usr/local/bin/teleport configure --roles=node \
  --auth-server="$(awk '/auth_server:/{print $2;exit}' $C)" \
  --token="$(awk '/token_name:/{print $2;exit}' $C)" \
  --node-name="$(awk '/nodename:/{print $2;exit}' $C)" \
  --node-labels=team=team_y,gpu=h200,env=prod \
  --output=file:///etc/teleport.yaml
systemctl restart teleport
```

（`deploy/k8s/nodes/teleport-agent.sh relabel` 就是这段逻辑。）

---

## 6. 上线检查清单

- [ ] `kubectl -n teleport get certificate` → Ready，dnsNames 含 `*` 
- [ ] DNSPod 里**主域名 + 泛域名**两条 CNAME 都指向当前 CLB
- [ ] Service 注解含 `specify-protocol`（443→TCP）和 `direct-access: "true"`
- [ ] `/webapi/ping` 连续 20 次全 200（集群内 + 外网各测一次）
- [ ] `/scripts/install.sh` 里有 `--base-url https://<cos>`
- [ ] COS 上 tar.gz / .sha256 / .sig 三者齐全，sha256 与本地一致
- [ ] `tctl get autoupdate_agent_rollout` 存在
- [ ] 新节点 `tctl nodes ls` 能看到，且 Labels 正确
- [ ] 清理掉所有临时排查资源（DaemonSet / 测试 Pod / 测试 token）

---

## 7. 常用命令

```bash
# 部署/升级 + 自检
cd deploy/k8s && ./deploy.sh install

# 只做健康检查
./deploy.sh verify

# 应用托管更新配置（会自动重启 auth）
./deploy.sh autoupdate

# 给用户绑超级管理员
./deploy.sh rbac <user>

# 批量装节点（需要能从本机 SSH 到目标机）
cd deploy/k8s/nodes && ./join-nodes.sh -f inventory.txt --new-token -c 10

# 单台机器安装脚本（适合批量执行平台下发）
bash teleport-agent.sh install --token=<token>
```

---

## 8. 教训

1. **云厂商的默认行为要显式覆盖**：TKE 的 443 七层、CLB 的 nodePort 路径、DNSPod 的 TXT 命名。
2. **"网络通"不等于"HTTP 通"**：`nc` 只能证明 SYN 能过，要测应用层 HTTP 才算数。
3. **诊断工具用完立刻删**：这次留下一个 `np-check` DaemonSet 跑了一整天。
4. **改集群名/换 CLB 这类"元数据"变更，比改代码更容易漏**，每次都要回头看 DNS 和证书。
