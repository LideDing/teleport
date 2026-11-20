# K8s 部署：Teleport 控制面运行在 TKE

> 场景：Teleport 控制面用官方 Helm chart `teleport-cluster` 18.11.1 部署在 TKE 集群里，镜像是本分支编译的 OSS 版本，agent 二进制放在腾讯云 COS。
> 单机部署（控制面运行在一台 Linux 机器上）是另一个独立场景，见 `deploy/single-host/README.md`；两者的对比见 `deploy/README.md`。

## 当前部署

| 项 | 值 |
| --- | --- |
| TKE 集群 | `cls-g8ta0gnm`（ap-singapore） |
| Helm chart | `teleport/teleport-cluster` 18.11.1，Release `teleport`，命名空间 `teleport` |
| 镜像 | `oitcep-tcr.tencentcloudcr.com/oitcep/teleport:v18.11.1-agpl`（私有 TCR） |
| 集群域名 | `teleport-agpl.ap-singapore.oitcep.com`（含泛域名） |
| 对外入口 | 腾讯云 CLB，443 强制为四层 TCP，CLB 直连 Pod |
| Proxy 监听模式 | `separate`：443 走 Web 和客户端，节点反向隧道走 3024 |
| TLS 证书 | cert-manager，ClusterIssuer `letsencrypt-dns`（DNS-01） |
| 数据存储 | `chartMode: standalone`：auth 单副本，数据存在 PVC |
| agent 二进制下载地址 | `https://teleport-agpl.cos.oitcep.com`，在 `values.yaml` 的 `extraEnv` 里设置 |

> `chartMode: standalone` 是 Helm chart 的存储模式，和"单机部署"场景无关。

## 文件

| 文件 | 用途 |
| --- | --- |
| `values.yaml` | Helm values：域名、镜像、下载地址、认证方式、证书、CLB 注解 |
| `deploy.sh` | 部署脚本：`install`、`verify`、`autoupdate`、`admin`、`rbac`、`status`、`uninstall` |
| `autoupdate.yaml` | `autoupdate_version` 和 `autoupdate_config`，开启 agent 托管更新 |
| `rbac-super-admin.yaml` | `super-admin` 角色 |
| `nodes/join-nodes.sh` | 通过 SSH 批量把 Linux 机器加入集群 |
| `nodes/teleport-agent.sh` | 单台机器的安装脚本，适合用批量执行平台下发 |
| `DEPLOYMENT_NOTES.md` | 踩坑记录和上线检查清单 |

## 前置条件

- 本机能用 kubectl 和 helm 操作 TKE 集群。
- 已登录私有镜像仓库，凭据在 `~/.docker/config.json`：`deploy.sh` 从这里取出该仓库的凭据，生成镜像拉取 Secret。
- 集群里已有 cert-manager 和 DNS-01 的 ClusterIssuer（`letsencrypt-dns`），以及负责域名解析的 external-dns。
- COS 桶允许匿名 GET，并通过 HTTPS 域名访问。

## 部署步骤

1. **编译、签名、上传 COS**：按仓库根目录 `SELF_HOSTED_COS_CDN.md` 第 3–6 节操作。签名密钥是仓库根目录的 `teleport-signing.key`。
2. **构建镜像**：用本分支编译的二进制构建 OSS 镜像，推送到 `values.yaml` 里的 `image`，tag 与 `teleportVersionOverride` 一致。镜像里只能放自编译的二进制，不能使用官方镜像或官方二进制。
3. **修改 `values.yaml`**：`clusterName`、`image` 和 `teleportVersionOverride`、`extraEnv` 里的 `TELEPORT_CDN_BASE_URL`、ClusterIssuer、CLB 安全组。
4. **部署**：

   ```bash
   cd deploy/k8s
   ./deploy.sh install      # helm upgrade --install，完成后自动执行 verify
   ```

5. **开启托管更新**：`./deploy.sh autoupdate`。它会应用 `autoupdate.yaml` 并重启 auth，让 `autoupdate_agent_rollout` 立即生成。当前 TKE 上的镜像早于提交 `6f6cf8ce532`，必须执行这一步，安装脚本才会带 `--base-url`；用本分支重建镜像后，这一步只用于开启自动升级。
6. **创建管理员**：

   ```bash
   ./deploy.sh admin           # 创建 admin（editor、access），输出注册链接
   ./deploy.sh rbac lideding   # 授予 super-admin（会覆盖该用户原有的角色）
   ```

7. **接入 Linux 节点**：

   ```bash
   cd nodes
   ./join-nodes.sh -f inventory.txt --new-token     # 通过 SSH 批量加入
   # 或者在目标机上执行：bash teleport-agent.sh install --token=TOKEN
   ```

8. **上线检查**：按 `DEPLOYMENT_NOTES.md` 第 6 节逐项确认。

## 日常运维

以下命令在 `deploy/k8s` 下执行：

| 操作 | 命令 |
| --- | --- |
| 健康检查 | `./deploy.sh verify` |
| 查看 Release、Pod、Service、证书 | `./deploy.sh status` |
| 执行 tctl | `kubectl -n teleport exec deploy/teleport-auth -- tctl <子命令>` |
| 升级版本 | 先把新版本产物上传 COS、推送新镜像，再改 `values.yaml` 的 `teleportVersionOverride`，执行 `CHART_VERSION=<新版本> ./deploy.sh install`；agent 的目标版本改 `autoupdate.yaml` 后执行 `./deploy.sh autoupdate` |
| 卸载 | `./deploy.sh uninstall`（保留 PVC 和命名空间） |
