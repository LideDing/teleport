# 本地编译产物验证:启动 Teleport 并通过 Web 访问

适用场景:内网 Linux server,无公网域名、无公网 TLS 证书,已通过
`TELEPORT_UPDATE_DEV_BUILD=1 make full` 构建出 `build/` 下的二进制,希望验证编译产物
能正常启动、Web UI 能正常渲染。

> 本文所有结论均可在本仓库源码中找到依据,末尾附文件与行号。

---

## 0. 确认产物与版本

```bash
cd /root/github.com/teleport

ls -lh build/
./build/teleport version
./build/tctl version
./build/tsh version
```

预期(以本次构建为例):

```
Teleport v19.0.0-prealpha.2 git:api/v19.0.0-prealpha.2-2104-g44bb87c2bd1 go1.26.7
```

`make full` 的产物为:`teleport`、`tctl`、`tsh`、`tbot`、`teleport-update`、
`fdpass-teleport`,以及内嵌到 `teleport` 的 `sessionhelper`。

---

## 1. 确认 Web UI 真的被内嵌(关键,否则会白屏)

`make full` 会强制 `WEBASSETS_SKIP_BUILD=0` 构建真实前端,而 `make all` 在某些情况下
只放一个占位 `index.html`。占位版的表现是:页面能打开但 JS 404、整页白屏。

先检查磁盘上的 webassets 是否为真实构建:

```bash
ls webassets/teleport/
find webassets/teleport/app -type f | wc -l
```

- 真实构建:`webassets/teleport/app/` 下有 **数百个** 文件(本机为 450 个),`index.html`
  引用的是打包后的资源。
- 占位版:只有 `webassets/teleport/index.html`,它引用 `/src/boot.tsx` 这类不存在的源文件 → 白屏。

再确认二进制带 `webassets_embed` 构建标签:

```bash
go version -m build/teleport 2>/dev/null | grep -o 'webassets_embed' | head -1
# 或者不依赖 go:
strings build/teleport | grep -c 'webassets/teleport/app' 
```

若 webassets 是占位版,重新执行一次 `TELEPORT_UPDATE_DEV_BUILD=1 make full`
(确保没有传 `WEBASSETS_SKIP_BUILD=1`)再继续。

---

## 2. 编写最小配置

内网环境**必须**显式设置 `public_addr` 为浏览器可达的服务器 IP。否则邀请链接和
Web UI 内部重定向会使用主机名,内网浏览器可能解析失败。

```yaml
# /etc/teleport.yaml(路径随意,启动时用 --config 指定;data_dir 需可写)
version: v3

teleport:
  data_dir: /var/lib/teleport
  # nodename: teleport-dev   # 可选,默认取主机名

auth_service:
  enabled: yes
  cluster_name: teleport-dev
  proxy_listener_mode: multiplex      # v3 默认即为 multiplex:SSH/Web 共用 3080
  # 纯本地验证可关闭 2FA,免去 TOTP App;若被拒绝则删除本节,改用默认 otp
  authentication:
    type: local
    second_factor: "off"

ssh_service:
  enabled: yes

proxy_service:
  enabled: yes
  web_listen_addr: 0.0.0.0:3080
  # 【内网必改】替换成这台服务器的内网 IP
  public_addr: ["192.168.1.10:3080"]
```

也可以让二进制生成一份基线配置再手工改:

```bash
./build/teleport configure \
  --output=file:///etc/teleport.yaml \
  --cluster-name=teleport-dev \
  --public-addr=192.168.1.10:3080 \
  --data-dir=/var/lib/teleport
```

> `public_addr` 不只是展示用:自签名证书的 SAN 会包含它,邀请链接也用它拼装。

---

## 3. 启动

前台启动,便于直接看日志:

```bash
./build/teleport start --config=/etc/teleport.yaml
```

内网无证书时,**Teleport 会自动生成自签名证书**,无需手工准备:

- 证书/私钥位置:`<data_dir>/webproxy_cert.pem`、`<data_dir>/webproxy_key.pem`、
  `<data_dir>/webproxy_pub.pem`
- 日志中会出现生成自签名证书的提示,属正常现象

另开一个终端确认端口:

```bash
ss -lntp | grep -E '3080|3025|3022'
curl -k -I https://127.0.0.1:3080/web
```

启动成功时日志会打印 Web UI 地址,形如 `https://192.168.1.10:3080`。

---

## 4. 创建可登录 Web UI 的用户

`tctl` 使用同一份配置和 `data_dir` 的本地管理员身份,不需要 `--insecure`:

```bash
./build/tctl --config=/etc/teleport.yaml users add alice --roles=editor,access
```

输出会包含一次性邀请链接,形如:

```
https://192.168.1.10:3080/web/invite/<token>
```

要点:

- `--roles` 是必填项;Web 登录建议 `editor,access`(`auditor` 只读)。
- 邀请链接默认有效期很短(约 1 小时),过期重新执行该命令即可。
- 若保留了 `second_factor: otp`(默认),首次注册需要 TOTP App 扫码;设为 `off` 则跳过。

---

## 5. 浏览器访问

在内网另一台机器上:

1. 打开 `https://192.168.1.10:3080/`(会自动跳转到 `/web`)。
2. 浏览器会提示自签名证书不受信任 → 选择"继续访问/高级 → 继续"。
3. 用第 4 步的邀请链接完成注册、设置密码,然后登录。

访问路径说明:根路径 `/` 会重定向到 `/web`,静态资源在 `/web/app/*`。

---

## 6. 排错

| 现象 | 原因 / 处理 |
|---|---|
| 页面白屏、控制台 JS 404 | webassets 是占位版。回到第 1 步,用 `WEBASSETS_SKIP_BUILD=0`(`make full` 默认)重建 |
| 邀请链接指向主机名而非 IP | 未设或设错 `public_addr`。改成浏览器可达的 `IP:3080` 后重启 |
| 浏览器连不上 3080 | 检查 `ss -lntp | grep 3080`;检查防火墙/安全组放行 3080 |
| 从别的机器 SSH/tsh 连不上 | multiplex 下 Web/SSH 代理走 3080;非 multiplex 需另开 3022/3023/3024/3025 |
| 登录时提示需要 MFA 但没有 App | 设 `auth_service.authentication.second_factor: "off"`,或安装 TOTP App |
| 以为 `teleport start --insecure` 能关掉 HTTPS | 不能。`--insecure` 只跳过**对外连接**的证书校验,不是浏览器侧开关 |
| 试了 `--insecure-no-tls` 后登录失败 | 该模式让 Web 变明文,但会话 Cookie 仍强制 `Secure`,通过 IP 访问会登录失败。内网请用自签名证书 + 忽略浏览器告警 |
| `permission denied` 写 data_dir | 用 root 运行,或把 `data_dir` 改到当前用户可写目录(如 `$HOME/teleport-data`) |

---

## 7. 停止与清理

```bash
# 前台运行时直接 Ctrl-C;若是后台:
pkill -f 'build/teleport.*start'

# 清空测试数据(证书、集群状态、用户都在 data_dir):
rm -rf /var/lib/teleport
```

---

## 8. 源码依据

- `teleport start` 不带配置即单机全角色(node/proxy/auth):`tool/teleport/common/usage.go:113-115`
- 无证书时自动生成自签名 HTTPS 证书:`lib/service/service.go:4808-4816`
- 自签名证书文件路径:`lib/defaults/defaults.go:634-638`
- 默认端口与监听地址(Web 3080、Auth 3025、SSH 3022/3023、隧道 3024、Kube 3026;绑定 `0.0.0.0`):`lib/defaults/defaults.go:44-62`、`lib/defaults/defaults.go:107`
- v2+ 配置默认 `proxy_listener_mode` 为 multiplex:`lib/config/fileconf.go:371-374`
- `teleport configure` 的 `--public-addr` / `--output` 等 flag:`tool/teleport/common/teleport.go:435-459`
- `tctl --config`:`tool/tctl/common/tctl.go:152-154`
- `tctl users add` 的 `--roles` 必填:`tool/tctl/common/user_command.go:115`
- 邀请链接格式 `https://<public_addr>/web/invite/<token>`:`lib/auth/usertoken.go:273-289`
- `second_factor` 取值与默认 `otp`:`api/constants/constants.go:245-265`
- `/` 重定向到 `/web`、静态资源 `/web/app/*`:`lib/web/apiserver.go:782-804`
- `webassets_embed` 与占位资源导致白屏的机制:`webassets_noembed.go:29-33`、`Makefile:1985`
- `make full` 强制构建 Web 资源:`Makefile:625-631`、`Makefile:424-426`
