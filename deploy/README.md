# Teleport 部署

本目录按部署场景组织。**K8s 部署和单机部署是两个独立的场景**：控制面的运行方式、证书、入口和 agent 二进制的分发方式都不同。两个场景的文档、配置和脚本分别放在 `k8s/` 和 `single-host/` 下，互不引用。

## 两个场景的区别

| | K8s 部署 | 单机部署 |
| --- | --- | --- |
| 目录 | `deploy/k8s/` | `deploy/single-host/` |
| 控制面运行在 | TKE 集群，官方 Helm chart `teleport-cluster` 18.11.1 | 一台 Linux 机器，systemd 服务 |
| Teleport 程序 | 自编译的 OSS 镜像（私有 TCR） | 自编译的二进制（`/usr/local/bin`） |
| 对外入口 | 腾讯云 CLB（四层 TCP） | 本机 443 端口 |
| Proxy 监听模式 | `separate`：443 + 反向隧道 3024 | `multiplex`：全部复用 443 |
| TLS 证书 | cert-manager（DNS-01） | certbot（DNS-01，tcc 插件） |
| agent 二进制下载地址 | 腾讯云 COS | 本机 NGINX 的 8443 端口 |
| 下载地址在哪设置 | `values.yaml` 的 `extraEnv` | 本机 `/etc/default/teleport` |
| 部署与管理方式 | `deploy.sh`（helm、kubectl） | 按文档手动执行，用 systemctl 和本机 tctl |
| 集群域名 | `teleport-agpl.ap-singapore.oitcep.com` | `teleport.devcloud.oitcep.com` |
| 入口文档 | `deploy/k8s/README.md` | `deploy/single-host/README.md` |

两点容易混淆：

- 单机部署也可以把 K8s 集群作为**资源**接入（在 K8s 里装 `teleport-kube-agent`），控制面仍然在那台机器上，这不是 K8s 部署。做法见 `deploy/single-host/QUICKSTART_COS.md` 第 5 节。
- `deploy/k8s/values.yaml` 里的 `chartMode: standalone` 是 Helm chart 的存储模式，和单机部署无关。

## 目录结构

```text
deploy/
├── README.md                     本文件
├── k8s/                          K8s 部署
│   ├── README.md                 部署步骤、文件说明
│   ├── DEPLOYMENT_NOTES.md       踩坑记录、上线检查清单
│   ├── values.yaml               Helm values
│   ├── deploy.sh                 部署脚本
│   ├── autoupdate.yaml           agent 托管更新
│   ├── rbac-super-admin.yaml     super-admin 角色
│   └── nodes/                    Linux 节点批量加入脚本
└── single-host/                  单机部署
    ├── README.md                 部署手册（本机 NGINX 分发）
    ├── QUICKSTART_COS.md         同一场景改用 COS 分发，并接入 Linux 节点、K8s 集群和 Web App
    ├── rbac-super-admin.yaml     super-admin 角色
    └── autoupdate.yaml           agent 托管更新（可选）
```

## 两个场景共用的部分

只有和部署方式无关的部分是共用的，都在仓库根目录：

- **源码**：服务端和 agent 都必须用本分支编译。提交 `6f6cf8ce532` 让 AGPL 构建始终按 `TELEPORT_CDN_BASE_URL` 安装 agent。
- **签名**：签名脚本 `sign-teleport-artifacts.sh` 和签名密钥 `teleport-signing.key`。两个场景目前用同一把密钥，所以同一份签名产物两边都能用。
- **原理参考**：`SELF_HOSTED_COS_CDN.md`，讲下载地址和签名校验的机制，以及用 COS 分发的完整步骤。

两个场景的 `rbac-super-admin.yaml` 和 `autoupdate.yaml` 各有一份，目前内容相同，修改其中一个场景的文件不影响另一个。
