#!/usr/bin/env bash
#
# 批量把 Linux 机器纳入 Teleport 管理（node 角色，反向隧道 + 托管更新）
#
# 用法:
#   ./join-nodes.sh -f inventory.txt --new-token            # 自动建 join token 并全量加入
#   ./join-nodes.sh -f inventory.txt -t <token>             # 用已有 token
#   ./join-nodes.sh -f inventory.txt --check                # 只检查远端状态
#   ./join-nodes.sh -f inventory.txt --dry-run              # 只打印将要执行的远端脚本
#
# 依赖: 本机有 ssh、kubectl（仅 --new-token / 集群校验时需要）
# 远端: root 或 NOPASSWD sudo、systemd、curl、tar、可访问 COS 与 Proxy:3024
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VALUES="${VALUES:-$SCRIPT_DIR/../values.yaml}"
INVENTORY=""
TOKEN=""
NEW_TOKEN=0
TOKEN_TTL="2h"
PROXY=""
TUNNEL_PORT="443"
WEB_PORT="443"
BASE_URL=""
VERSION=""
SSH_USER="root"
SSH_PORT="22"
SSH_KEY=""
SSH_OPTS=""
CONCURRENCY=5
FORCE=0
DRY_RUN=0
CHECK_ONLY=0
LOGDIR="${LOGDIR:-$SCRIPT_DIR/logs}"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarn\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31merr \033[0m %s\n' "$*" >&2; exit 1; }

usage() { sed -n '3,13p' "$0"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    -f|--file)       INVENTORY="$2"; shift 2 ;;
    -t|--token)      TOKEN="$2"; shift 2 ;;
    --new-token)     NEW_TOKEN=1; shift ;;
    --token-ttl)     TOKEN_TTL="$2"; shift 2 ;;
    -p|--proxy)      PROXY="$2"; shift 2 ;;
    --tunnel-port)   TUNNEL_PORT="$2"; shift 2 ;;
    -b|--base-url)   BASE_URL="$2"; shift 2 ;;
    --version)       VERSION="$2"; shift 2 ;;
    --ssh-user)      SSH_USER="$2"; shift 2 ;;
    --ssh-port)      SSH_PORT="$2"; shift 2 ;;
    --ssh-key)       SSH_KEY="$2"; shift 2 ;;
    --ssh-opts)      SSH_OPTS="$2"; shift 2 ;;
    -c|--concurrency) CONCURRENCY="$2"; shift 2 ;;
    --force)         FORCE=1; shift ;;
    --dry-run)       DRY_RUN=1; shift ;;
    --check)         CHECK_ONLY=1; shift ;;
    --log-dir)       LOGDIR="$2"; shift 2 ;;
    -h|--help)       usage; exit 0 ;;
    *) die "未知参数: $1（-h 查看用法）" ;;
  esac
done

[[ -n "$INVENTORY" ]] || die "必须指定 -f inventory.txt"
[[ -f "$INVENTORY" ]] || die "找不到 inventory: $INVENTORY"

# 从 k8s 的 values.yaml 里取默认值，避免两处维护
value_of() { awk -v k="^$1:" '$0 ~ k {print $2; exit}' "$VALUES" 2>/dev/null | tr -d '"'; }
[[ -n "$PROXY" ]]    || PROXY="$(value_of clusterName)"
[[ -n "$BASE_URL" ]] || BASE_URL="$(awk '/^[[:space:]]*value:/{print $2; exit}' "$VALUES" 2>/dev/null | tr -d '"')"
[[ -n "$VERSION" ]]  || VERSION="$(awk '/^teleportVersionOverride:/{print $2; exit}' "$VALUES" 2>/dev/null | sed -E 's/^"?v?([0-9]+\.[0-9]+\.[0-9]+).*/\1/')"
[[ -n "$VERSION" ]]  || VERSION="18.11.1"
[[ -n "$PROXY" ]]    || die "无法确定 Proxy 地址，请用 --proxy 指定"
[[ "$BASE_URL" == http* ]] || die "无法确定 COS 地址，请用 --base-url 指定"

# 需要 token（除非只做检查）
if [[ $CHECK_ONLY -eq 0 && -z "$TOKEN" ]]; then
  if [[ $DRY_RUN -eq 1 ]]; then
    TOKEN="DRY-RUN-TOKEN"
  elif [[ $NEW_TOKEN -eq 1 ]]; then
    command -v kubectl >/dev/null || die "生成 token 需要 kubectl"
    log "创建 node join token（TTL $TOKEN_TTL）"
    TOKEN="$(kubectl -n teleport exec deploy/teleport-auth -- tctl tokens add --type=node --ttl "$TOKEN_TTL" 2>/dev/null \
      | awk '/The invite token/ {print $NF}')"
    [[ -n "$TOKEN" ]] || die "创建 token 失败"
    log "token: $TOKEN（$TOKEN_TTL 后失效，用完可 tctl tokens rm）"
  else
    die "需要 --token 或 --new-token"
  fi
fi

ssh_cmd() {
  local host="$1"; shift
  local opts=(-o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 -p "$SSH_PORT")
  [[ -n "$SSH_KEY" ]] && opts+=(-i "$SSH_KEY")
  # shellcheck disable=SC2086
  ssh "${opts[@]}" $SSH_OPTS "$SSH_USER@$host" "$@"
}

# 远端执行的脚本：安装 teleport-update、写 teleport.yaml、启动 node
render_install_script() {
  cat <<'EOS'
set -euo pipefail
BASE="@@BASE@@"; PROXY="@@PROXY@@"; TUNNEL_PORT="@@TUNNEL_PORT@@"
WEB_PORT="@@WEB_PORT@@"; TOKEN="@@TOKEN@@"; LABELS="@@LABELS@@"
NODENAME="@@NODENAME@@"; VERSION="@@VERSION@@"; FORCE="@@FORCE@@"

[[ "$(id -u)" -eq 0 ]] || { echo "FATAL: 需要 root（用 root 或 NOPASSWD sudo）"; exit 10; }
for c in curl tar systemctl; do command -v "$c" >/dev/null || { echo "FATAL: 缺少 $c"; exit 11; }; done

if [[ "$FORCE" != "1" ]] && systemctl is-active --quiet teleport 2>/dev/null \
   && grep -q "auth_server: ${PROXY}:${TUNNEL_PORT}" /etc/teleport.yaml 2>/dev/null; then
  echo "ALREADY_JOINED"
  exit 0
fi

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "FATAL: 不支持的架构 $(uname -m)"; exit 12 ;;
esac

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
URL="${BASE}/teleport-v${VERSION}-linux-${ARCH}-bin.tar.gz"
echo "downloading $URL"
curl -fsSL --retry 3 --connect-timeout 10 "$URL" -o "$TMP/tp.tgz"
tar xzf "$TMP/tp.tgz" -C "$TMP"

install -m 0755 "$TMP/teleport/teleport-update" /usr/local/bin/teleport-update

# 用解包出来的二进制生成配置，避免和 teleport-update 管理的 /usr/local/bin/teleport 抢文件
CONFIG_ARGS=(--roles=node --auth-server="${PROXY}:${TUNNEL_PORT}" --token="$TOKEN")
[[ -n "$LABELS" ]]   && CONFIG_ARGS+=(--node-labels="$LABELS")
[[ -n "$NODENAME" ]] && CONFIG_ARGS+=(--node-name="$NODENAME")
"$TMP/teleport/teleport" configure "${CONFIG_ARGS[@]}" --output=file:///etc/teleport.yaml

# --proxy 用 Web 端口查版本，--auth-server 用隧道端口加入集群；
# 端口不一致时（例如 separate 模式用 3024）必须显式允许冲突
EXTRA=()
[[ "$TUNNEL_PORT" != "$WEB_PORT" ]] && EXTRA+=(--allow-proxy-conflict)
teleport-update enable --proxy "${PROXY}:${WEB_PORT}" --base-url "$BASE" "${EXTRA[@]}" \
  --overwrite --log-format=text

systemctl daemon-reload
systemctl enable teleport >/dev/null 2>&1
systemctl restart teleport
sleep 3
systemctl is-active --quiet teleport || { echo "FATAL: teleport 服务未启动"; systemctl status teleport --no-pager -l | tail -20; exit 13; }
echo "JOINED: $(hostname) labels=${LABELS:-none} auth=${PROXY}:${TUNNEL_PORT}"
teleport-update status 2>&1 | tail -3 || true
EOS
}

render_check_script() {
  cat <<'EOS'
set -uo pipefail
echo "host: $(hostname)"
echo "os: $(. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME")"
echo -n "teleport.service: "; systemctl is-active teleport 2>/dev/null || true
if [[ -f /etc/teleport.yaml ]]; then
  grep -E '^\s*(nodename|auth_server):' /etc/teleport.yaml || true
  echo -n "labels: "; sed -n '/^ssh_service:/,/^[a-z]/p' /etc/teleport.yaml | grep -A20 'labels:' | grep -E '^\s{4}\w' | tr -d ' ' | paste -sd, - || true
else
  echo "no /etc/teleport.yaml"
fi
echo -n "teleport version: "; teleport version 2>/dev/null | head -1 || echo none
echo "--- teleport-update status ---"
teleport-update status 2>&1 | tail -5 || true
EOS
}

fill() {
  local s="$1"; shift
  while [[ $# -ge 2 ]]; do
    s="${s//@@$1@@/$2}"; shift 2
  done
  printf '%s' "$s"
}

run_host() {
  local host="$1" labels="$2"
  local nodename="" out logf statusf
  logf="$LOGDIR/$host.log"; statusf="$LOGDIR/$host.status"
  local script
  if [[ $CHECK_ONLY -eq 1 ]]; then
    script="$(render_check_script)"
  else
    script="$(fill "$(render_install_script)" BASE "$BASE_URL" PROXY "$PROXY" TUNNEL_PORT "$TUNNEL_PORT" \
      WEB_PORT "$WEB_PORT" TOKEN "$TOKEN" LABELS "$labels" NODENAME "$nodename" VERSION "$VERSION" FORCE "$FORCE")"
  fi

  if [[ $DRY_RUN -eq 1 ]]; then
    printf '===== %s =====\n%s\n' "$host" "$script" > "$logf"
    echo "DRY_RUN $host -> $logf"
    echo "dry" > "$statusf"
    return 0
  fi

  if out="$(printf '%s' "$script" | ssh_cmd "$host" 'if [ "$(id -u)" -eq 0 ]; then bash -s; else sudo -n bash -s; fi' 2>&1)"; then
    printf '%s\n' "$out" > "$logf"
    if grep -q ALREADY_JOINED <<<"$out"; then
      echo "SKIP  $host（已加入）"; echo "already" > "$statusf"
    elif [[ $CHECK_ONLY -eq 1 ]]; then
      echo "CHECK $host"; echo "ok" > "$statusf"
    else
      echo "OK    $host"; echo "ok" > "$statusf"
    fi
  else
    printf '%s\n' "$out" > "$logf"
    echo "FAIL  $host（见 $logf）"; echo "fail" > "$statusf"
  fi
}

mkdir -p "$LOGDIR"
log "inventory=$INVENTORY proxy=$PROXY (web:$WEB_PORT tunnel:$TUNNEL_PORT) cdn=$BASE_URL version=$VERSION"
log "并发=$CONCURRENCY 日志目录=$LOGDIR"

hosts=()
while IFS= read -r line; do
  line="${line%%#*}"
  [[ -z "${line// }" ]] && continue
  read -r h l <<<"$line"
  [[ -n "$h" ]] && hosts+=("$h|${l:-}")
done < "$INVENTORY"
[[ ${#hosts[@]} -gt 0 ]] || die "inventory 为空"

pids=()
for item in "${hosts[@]}"; do
  h="${item%%|*}"; l="${item#*|}"
  run_host "$h" "$l" &
  pids+=($!)
  if (( ${#pids[@]} >= CONCURRENCY )); then wait -n || true; pids=($(jobs -p | tr '\n' ' ')); fi
done
wait || true

ok=0; fail=0; skip=0; dry=0
for f in "$LOGDIR"/*.status; do
  case "$(cat "$f")" in
    ok) ok=$((ok+1)) ;;
    fail) fail=$((fail+1)) ;;
    already) skip=$((skip+1)) ;;
    dry) dry=$((dry+1)) ;;
  esac
done
[[ $dry -gt 0 ]] && { log "dry-run 输出 $dry 份"; exit 0; }
echo
log "结果: 成功 $ok / 已存在 $skip / 失败 $fail（共 ${#hosts[@]} 台）"
[[ $fail -eq 0 ]] || { warn "失败机器的日志在 $LOGDIR/<host>.log"; exit 1; }

if [[ $CHECK_ONLY -eq 0 && $DRY_RUN -eq 0 ]] && command -v kubectl >/dev/null; then
  echo
  log "集群侧节点列表"
  kubectl -n teleport exec deploy/teleport-auth -- tctl nodes ls 2>/dev/null | head -30 || true
fi
