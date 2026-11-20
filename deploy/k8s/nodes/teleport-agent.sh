#!/usr/bin/env bash
#
# 单台机器安装脚本：把一台 Linux 机器加入 K8s 部署的 Teleport 集群（node 角色，托管更新）
# 适合通过 SecureCRT / JumpServer / 批量执行平台一次性下发到多台主机。
#
# 用法（目标机上以 root 执行）：
#   bash teleport-agent.sh preflight                 # 只检查：网络 + 下载 + 校验和
#   bash teleport-agent.sh install --token=xxxx      # 安装并加入集群
#   bash teleport-agent.sh relabel --labels=a=b      # 只更新节点标签并重启
#   bash teleport-agent.sh status                    # 查看状态
#   bash teleport-agent.sh remove                    # 卸载（保留数据目录）
#   PURGE=1 bash teleport-agent.sh remove            # 连 /var/lib/teleport 一起删
#
# 可用参数（也可用同名环境变量）：
#   --proxy        Teleport Proxy 域名    (默认 teleport-agpl.ap-singapore.oitcep.com)
#   --cdn          二进制分发地址          (默认 https://teleport-agpl.cos.oitcep.com)
#   --version      Teleport 版本           (默认 18.11.1)
#   --token        join token（install 必填）
#   --tunnel-port  加入集群用端口          (默认 443)
#   --web-port     更新器查询版本用端口    (默认 443)
#   --labels       节点标签 env=prod,role=web
#   --nodename     节点名（默认系统 hostname）
#   --force        已安装也重新执行
#
set -euo pipefail

PROXY="${TELEPORT_PROXY:-teleport-agpl.ap-singapore.oitcep.com}"
CDN="${TELEPORT_CDN:-https://teleport-agpl.cos.oitcep.com}"
VERSION="${TELEPORT_VERSION:-18.11.1}"
TOKEN="${TELEPORT_TOKEN:-}"
TUNNEL_PORT="${TELEPORT_TUNNEL_PORT:-443}"
WEB_PORT="${TELEPORT_WEB_PORT:-443}"
# 默认标签（按需用 --labels= 覆盖）
LABELS="${NODE_LABELS:-team=team_y,gpu=h200,env=prod}"
NODENAME="${NODENAME:-}"
FORCE="${FORCE:-0}"
PURGE="${PURGE:-0}"
ACTION="install"

for arg in "$@"; do
  case "$arg" in
    --proxy=*)       PROXY="${arg#*=}" ;;
    --cdn=*)         CDN="${arg#*=}" ;;
    --version=*)     VERSION="${arg#*=}" ;;
    --token=*)       TOKEN="${arg#*=}" ;;
    --tunnel-port=*) TUNNEL_PORT="${arg#*=}" ;;
    --web-port=*)    WEB_PORT="${arg#*=}" ;;
    --labels=*)      LABELS="${arg#*=}" ;;
    --nodename=*)    NODENAME="${arg#*=}" ;;
    --force)         FORCE=1 ;;
    --purge)         PURGE=1 ;;
    install|status|remove|preflight|relabel) ACTION="$arg" ;;
    -h|--help)       sed -n '3,25p' "$0"; exit 0 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

ts() { date -u '+%Y-%m-%d %H:%M:%S UTC'; }
say()  { printf '%s [teleport-agent] %s\n' "$(ts)" "$*"; }
warn() { printf '%s [teleport-agent] WARN: %s\n' "$(ts)" "$*" >&2; }
die()  { printf '%s [teleport-agent] ERROR: %s\n' "$(ts)" "$*" >&2; exit 1; }

need_root() { [[ "$(id -u)" -eq 0 ]] || die "需要 root 执行（sudo -i 或 root 登录）"; }

have() { command -v "$1" >/dev/null 2>&1; }

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) die "不支持的架构: $(uname -m)" ;;
  esac
}

TARBALL_URL() { echo "${CDN}/teleport-v${VERSION}-linux-${ARCH}-bin.tar.gz"; }

fetch() {  # fetch <url> <out>
  if have curl; then
    curl -fsSL --retry 5 --retry-delay 2 --retry-all-errors --connect-timeout 10 "$1" -o "$2"
  elif have wget; then
    wget -q -T 15 -t 5 -O "$2" "$1"
  else
    die "缺少 curl/wget"
  fi
}

check_proxy() {
  local code
  if have curl; then
    code="$(curl -sk -o /dev/null -m 8 -w '%{http_code}' "https://${PROXY}:${WEB_PORT}/webapi/ping" || true)"
  else
    code="$(wget -q --no-check-certificate -T 8 -O /dev/null "https://${PROXY}:${WEB_PORT}/webapi/ping" && echo 200 || echo 000)"
  fi
  [[ "$code" == "200" ]] || die "无法访问 https://${PROXY}:${WEB_PORT}/webapi/ping（返回 $code），检查网络/防火墙/DNS"
  say "Proxy 连通性正常（$PROXY:$WEB_PORT）"
}

download_and_verify() {  # download_and_verify <dir> -> sets TGZ
  local dir="$1" sum_url want got
  TGZ="$dir/teleport.tgz"
  local url; url="$(TARBALL_URL)"
  say "下载 $url"
  fetch "$url" "$TGZ"

  sum_url="${url}.sha256"
  if fetch "$sum_url" "$dir/tp.sha256" 2>/dev/null; then
    want="$(head -c 64 "$dir/tp.sha256" | tr -d ' \n' | tr 'A-F' 'a-f')"
    got="$(sha256sum "$TGZ" | awk '{print $1}')"
    [[ "$want" == "$got" ]] || die "SHA256 校验失败（期望 $want，实际 $got）"
    say "SHA256 校验通过"
  else
    warn "CDN 上没有 .sha256，跳过校验"
  fi
}

do_preflight() {
  need_root
  ARCH="$(detect_arch)"
  say "host=$(hostname) arch=$ARCH os=$(. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME")"
  check_proxy
  local tmp; tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
  download_and_verify "$tmp"
  tar tzf "$TGZ" >/dev/null || die "压缩包损坏"
  say "预检通过，可以执行 install"
}

do_install() {
  need_root
  have systemctl || die "没有 systemd，当前脚本只支持 systemd 系统"
  [[ -n "$TOKEN" ]] || die "缺少 --token（先在集群执行 tctl tokens add --type=node --ttl 2h）"

  ARCH="$(detect_arch)"
  say "host=$(hostname) arch=$ARCH labels=${LABELS:-none}"

  if [[ "$FORCE" != "1" ]] && systemctl is-active --quiet teleport 2>/dev/null \
     && grep -q "auth_server: ${PROXY}:${TUNNEL_PORT}" /etc/teleport.yaml 2>/dev/null; then
    say "ALREADY_JOINED：本机已在运行且 auth_server 匹配，跳过（--force 可强制重装）"
    return 0
  fi

  check_proxy

  local tmp; tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
  download_and_verify "$tmp"
  tar xzf "$TGZ" -C "$tmp"

  install -m 0755 "$tmp/teleport/teleport-update" /usr/local/bin/teleport-update

  local args=(--roles=node --auth-server="${PROXY}:${TUNNEL_PORT}" --token="$TOKEN" --output=file:///etc/teleport.yaml)
  [[ -n "$LABELS" ]]   && args+=(--node-labels="$LABELS")
  [[ -n "$NODENAME" ]] && args+=(--node-name="$NODENAME")
  "$tmp/teleport/teleport" configure "${args[@]}"
  say "已写入 /etc/teleport.yaml"

  local extra=(); [[ "$TUNNEL_PORT" != "$WEB_PORT" ]] && extra+=(--allow-proxy-conflict)
  local i
  for i in 1 2 3; do
    if teleport-update enable --proxy "${PROXY}:${WEB_PORT}" --base-url "$CDN" "${extra[@]}" \
         --overwrite --log-format=text; then
      break
    fi
    [[ $i -eq 3 ]] && die "teleport-update enable 连续 3 次失败"
    warn "teleport-update enable 失败，5 秒后重试（$i/3）"
    sleep 5
  done

  systemctl daemon-reload
  systemctl enable teleport >/dev/null 2>&1
  systemctl restart teleport
  for i in $(seq 1 15); do
    systemctl is-active --quiet teleport && break
    sleep 2
  done
  systemctl is-active --quiet teleport || {
    systemctl status teleport --no-pager -l | tail -30
    die "teleport 服务未启动"
  }

  say "JOINED host=$(hostname) auth=${PROXY}:${TUNNEL_PORT} labels=${LABELS:-none}"
  teleport-update status 2>&1 | tail -3 || true
}

do_relabel() {
  need_root
  [[ -f /etc/teleport.yaml ]] || die "本机没有 /etc/teleport.yaml，请先 install"
  [[ -n "$LABELS" ]] || die "缺少 --labels"
  have /usr/local/bin/teleport || die "找不到 /usr/local/bin/teleport"

  local auth token nodename
  auth="$(awk '/^[[:space:]]*auth_server:/{print $2; exit}' /etc/teleport.yaml | tr -d '"')"
  token="$(awk '/token_name:/{print $2; exit}' /etc/teleport.yaml | tr -d '"')"
  nodename="$(awk '/^[[:space:]]*nodename:/{print $2; exit}' /etc/teleport.yaml | tr -d '"')"
  [[ -n "$auth" && -n "$token" ]] || die "无法从 /etc/teleport.yaml 解析 auth_server / token_name"

  say "relabel host=$(hostname) labels=$LABELS (auth=$auth)"
  local args=(--roles=node --auth-server="$auth" --token="$token" --node-labels="$LABELS" --output=file:///etc/teleport.yaml)
  [[ -n "$nodename" ]] && args+=(--node-name="$nodename")
  /usr/local/bin/teleport configure "${args[@]}"

  systemctl restart teleport
  local i
  for i in $(seq 1 15); do systemctl is-active --quiet teleport && break; sleep 2; done
  systemctl is-active --quiet teleport || die "teleport 重启失败"
  say "RELABELED host=$(hostname) labels=$LABELS"
}

do_status() {
  say "host=$(hostname)"
  . /etc/os-release 2>/dev/null && say "os=$PRETTY_NAME"
  if [[ -f /etc/teleport.yaml ]]; then
    grep -E '^\s+(nodename|auth_server):' /etc/teleport.yaml 2>/dev/null || true
  else
    say "未安装（没有 /etc/teleport.yaml）"
    return 0
  fi
  printf 'service: '; systemctl is-active teleport 2>/dev/null || true
  printf 'binary:  '; /usr/local/bin/teleport version 2>/dev/null | head -1 || echo none
  command -v teleport-update >/dev/null && teleport-update status 2>&1 | tail -5 || true
}

do_remove() {
  need_root
  say "停止并移除 Teleport"
  if have teleport-update; then teleport-update disable >/dev/null 2>&1 || true; fi
  systemctl disable --now teleport >/dev/null 2>&1 || true
  rm -f /etc/systemd/system/teleport.service /etc/systemd/system/teleport-update.timer /etc/systemd/system/teleport-update.service
  rm -f /usr/local/bin/teleport /usr/local/bin/teleport-update /usr/local/bin/tctl /usr/local/bin/tsh
  systemctl daemon-reload
  if [[ "$PURGE" == "1" ]]; then
    say "PURGE=1：删除 /etc/teleport.yaml 和 /var/lib/teleport"
    rm -rf /etc/teleport.yaml /var/lib/teleport
  else
    say "保留 /etc/teleport.yaml 与 /var/lib/teleport（PURGE=1 可一并删除）"
  fi
  say "REMOVED"
}

case "$ACTION" in
  preflight) do_preflight ;;
  install)   do_install ;;
  relabel)   do_relabel ;;
  status)    do_status ;;
  remove)    do_remove ;;
  *) die "未知动作: $ACTION" ;;
esac
