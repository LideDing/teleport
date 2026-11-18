#!/usr/bin/env bash
#
# Teleport v18.11.1 (self-built OSS image) on TKE with Tencent COS as binary CDN.
#
# 用法:
#   ./deploy.sh install      # 部署/升级并等待就绪（默认）
#   ./deploy.sh verify       # 只做校验（Pod / 证书 / LB / 安装脚本）
#   ./deploy.sh autoupdate   # 应用 autoupdate_config + autoupdate_version（让安装脚本带 --base-url）
#   ./deploy.sh admin        # 创建管理员用户（输出一次性注册链接）
#   ./deploy.sh rbac [用户]  # 给用户绑定 super-admin（全资源 + root + k8s cluster-admin）
#   ./deploy.sh status       # 查看 Release / Pod / Service / 证书
#   ./deploy.sh uninstall    # 卸载（保留 PVC）
#
# 可覆盖的环境变量:
#   RELEASE / NAMESPACE / CHART_VERSION / VALUES / PULL_SECRET / TIMEOUT
#
set -euo pipefail

RELEASE="${RELEASE:-teleport}"
NAMESPACE="${NAMESPACE:-teleport}"
CHART="${CHART:-teleport/teleport-cluster}"
CHART_VERSION="${CHART_VERSION:-18.11.1}"
REPO_NAME="${REPO_NAME:-teleport}"
REPO_URL="${REPO_URL:-https://charts.releases.teleport.dev}"
PULL_SECRET="${PULL_SECRET:-teleport-registry}"
TIMEOUT="${TIMEOUT:-10m}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VALUES="${VALUES:-$SCRIPT_DIR/values.yaml}"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarn\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31merr \033[0m %s\n' "$*" >&2; exit 1; }

require_cmds() {
  command -v kubectl >/dev/null || die "缺少 kubectl"
  command -v helm    >/dev/null || die "缺少 helm"
  [[ -f "$VALUES" ]] || die "找不到 values 文件: $VALUES"
}

hostname_from_values() {
  awk '/^clusterName:/{print $2; exit}' "$VALUES" | tr -d '"'
}

image_registry() {
  awk '/^image:/{print $2; exit}' "$VALUES" | cut -d/ -f1
}

ensure_repo() {
  helm repo list 2>/dev/null | awk '{print $1}' | grep -qx "$REPO_NAME" \
    || helm repo add "$REPO_NAME" "$REPO_URL" >/dev/null
  helm repo update "$REPO_NAME" >/dev/null
  local found
  found="$(helm search repo "$REPO_NAME/teleport-cluster" --version "$CHART_VERSION" -o json 2>/dev/null || true)"
  [[ -n "$found" ]] || die "官方仓库里没有 teleport-cluster $CHART_VERSION"
}

ensure_pull_secret() {
  if kubectl -n "$NAMESPACE" get secret "$PULL_SECRET" >/dev/null 2>&1; then
    log "镜像拉取凭据 $NAMESPACE/$PULL_SECRET 已存在，跳过"
    return
  fi

  local registry src tmp
  registry="$(image_registry)"
  src="${DOCKER_CONFIG:-$HOME/.docker}/config.json"
  [[ -f "$src" ]] || die "没有 $src，请先 podman login $registry"

  tmp="$(mktemp)"
  python3 - "$src" "$registry" >"$tmp" <<'PY' || { rm -f "$tmp"; die "从 $src 里取不到 $registry 的凭据，请先 podman login"; }
import json, sys
cfg = json.load(open(sys.argv[1]))
reg = sys.argv[2]
auths = cfg.get("auths") or {}
if reg not in auths:
    sys.exit(1)
json.dump({"auths": {reg: auths[reg]}}, sys.stdout)
PY
  kubectl -n "$NAMESPACE" create secret generic "$PULL_SECRET" \
    --type=kubernetes.io/dockerconfigjson \
    --from-file=.dockerconfigjson="$tmp" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  rm -f "$tmp"
  log "已创建镜像拉取凭据 $NAMESPACE/$PULL_SECRET"
}

do_install() {
  require_cmds
  kubectl cluster-info >/dev/null 2>&1 || die "kubectl 连不上集群"

  local host
  host="$(hostname_from_values)"
  [[ -n "$host" ]] || die "values.yaml 里没有 clusterName"
  if grep -q 'CHANGE-ME' "$VALUES"; then
    warn "TELEPORT_CDN_BASE_URL 还是占位符，安装脚本会指向不存在的地址（改 values.yaml 后重跑本脚本即可）"
  fi

  ensure_repo
  kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  ensure_pull_secret

  log "helm upgrade --install $RELEASE (chart $CHART_VERSION) 到 $NAMESPACE"
  # shellcheck disable=SC2086
  helm upgrade --install "$RELEASE" "$CHART" \
    --version "$CHART_VERSION" \
    --namespace "$NAMESPACE" \
    -f "$VALUES" \
    --wait --atomic --timeout "$TIMEOUT" \
    ${EXTRA_HELM_ARGS:-}

  log "部署完成，开始校验"
  do_verify
}

# 在集群里跑一次性 curl Pod，避免本地网络/代理干扰
cluster_probe() {
  local cip="$1" ph=""
  kubectl -n "$NAMESPACE" delete pod teleport-verify --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NAMESPACE" run teleport-verify --image=curlimages/curl:latest --restart=Never \
    --command -- sh -c "curl -sk -o /dev/null -w 'ping=%{http_code}\\n' --max-time 10 --resolve $host:443:$cip https://$host/webapi/ping; curl -s --max-time 10 --resolve $host:443:$cip https://$host/scripts/install.sh" >/dev/null 2>&1
  for _ in $(seq 1 20); do
    ph="$(kubectl -n "$NAMESPACE" get pod teleport-verify -o jsonpath='{.status.phase}' 2>/dev/null)"
    case "$ph" in Succeeded|Failed) break ;; esac
    sleep 3
  done
  kubectl -n "$NAMESPACE" logs teleport-verify 2>/dev/null
  kubectl -n "$NAMESPACE" delete pod teleport-verify --ignore-not-found >/dev/null 2>&1
}

do_verify() {
  require_cmds
  local host ip lbhost code script out cip
  host="$(hostname_from_values)"

  log "Pod 状态"
  kubectl -n "$NAMESPACE" get pods -o wide

  log "等待 Deployment 就绪"
  kubectl -n "$NAMESPACE" rollout status "deploy/$RELEASE-auth"  --timeout=300s || true
  kubectl -n "$NAMESPACE" rollout status "deploy/$RELEASE-proxy" --timeout=300s

  log "等待证书 $RELEASE 就绪"
  if kubectl -n "$NAMESPACE" get certificate "$RELEASE" >/dev/null 2>&1; then
    kubectl -n "$NAMESPACE" wait --for=condition=Ready "certificate/$RELEASE" --timeout=300s \
      || warn "证书还没 Ready，稍后用 kubectl -n $NAMESPACE describe certificate/$RELEASE 排查"
  else
    warn "没找到 certificate/$RELEASE（可能用的是自签证书）"
  fi

  log "LoadBalancer 地址"
  ip="$(kubectl -n "$NAMESPACE" get svc "$RELEASE" -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
  lbhost="$(kubectl -n "$NAMESPACE" get svc "$RELEASE" -o jsonpath='{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null || true)"
  if [[ -z "$ip" && -n "$lbhost" ]]; then
    # 腾讯云 CLB 常以域名形式回填，本地解析一次拿 IP 用于 --resolve
    ip="$(python3 -c 'import socket,sys;print(socket.gethostbyname(sys.argv[1]))' "$lbhost" 2>/dev/null || true)"
    echo "    $host -> $lbhost${ip:+ ($ip)}"
  elif [[ -n "$ip" ]]; then
    echo "    $host -> $ip"
  fi
  [[ -n "$ip" ]] || { warn "Service 还没有外部地址"; return 0; }

  log "集群内探活（走 ClusterIP，排除本机网络干扰）"
  cip="$(kubectl -n "$NAMESPACE" get svc "$RELEASE" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)"
  if [[ -n "$cip" ]]; then
    out="$(cluster_probe "$cip")"
    code="$(sed -n 's/.*ping=\([0-9]*\).*/\1/p' <<<"$out" | head -1)"
    [[ "$code" == "200" ]] && echo "    /webapi/ping 200" || warn "/webapi/ping 返回 ${code:-无响应}"

    script="$(sed '1d' <<<"$out")"
    if grep -q -- '--base-url' <<<"$script"; then
      grep -o -- '--base-url [^ ]*' <<<"$script" | head -1 | sed 's/^/    install.sh: /'
    elif [[ -n "$script" ]]; then
      warn "install.sh 里没有 --base-url：先执行 ./deploy.sh autoupdate，再重启 auth（rollout 控制器默认同步较慢）"
    fi
  fi

  log "本机探活（可能被公司网络/代理拦截，仅供参考）"
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
    --resolve "$host:443:$ip" "https://$host/webapi/ping" || true)"
  [[ "$code" == "200" ]] && echo "    /webapi/ping 200" || warn "本机访问返回 $code（如集群内探活正常可忽略）"

  echo
  log "后续步骤"
  echo "    浏览器打开 https://$host （DNS 生效后）"
  echo "    创建管理员: ./deploy.sh admin"
}

do_autoupdate() {
  require_cmds
  local pod
  pod="$(kubectl -n "$NAMESPACE" get pod -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=auth" \
    -o jsonpath='{.items[0].metadata.name}')"
  [[ -n "$pod" ]] || die "找不到 auth pod"
  log "在 $pod 中应用 autoupdate 资源"
  kubectl -n "$NAMESPACE" exec -i "$pod" -- tctl create -f - < "$SCRIPT_DIR/autoupdate.yaml"

  # auth 里的 rollout 控制器默认同步周期很长，重启一次让它立刻把 config+version
  # 合并成 autoupdate-agent-rollout（没有这个单例，/scripts/install.sh 会退回旧版脚本、
  # 不带 --base-url）
  log "重启 auth 触发 rollout 同步"
  kubectl -n "$NAMESPACE" rollout restart "deploy/$RELEASE-auth" >/dev/null
  kubectl -n "$NAMESPACE" rollout status "deploy/$RELEASE-auth" --timeout=300s
  kubectl -n "$NAMESPACE" exec -i "$(kubectl -n "$NAMESPACE" get pod -l app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=auth -o jsonpath='{.items[0].metadata.name}')" \
    -- tctl get autoupdate_agent_rollout | head -10
  log "完成：/scripts/install.sh 现在会带 --base-url"
}

do_admin() {
  require_cmds
  local pod user
  user="${ADMIN_USER:-admin}"
  pod="$(kubectl -n "$NAMESPACE" get pod -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=auth" \
    -o jsonpath='{.items[0].metadata.name}')"
  [[ -n "$pod" ]] || die "找不到 auth pod"
  kubectl -n "$NAMESPACE" exec -i "$pod" -- tctl -c /etc/teleport/teleport.yaml users add "$user" \
    --roles=editor,access --logins=root
}

do_rbac() {
  require_cmds
  local user="${1:-lideding}" pod
  pod="$(kubectl -n "$NAMESPACE" get pod -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=auth" \
    -o jsonpath='{.items[0].metadata.name}')"
  [[ -n "$pod" ]] || die "找不到 auth pod"

  log "应用 super-admin 角色（全部资源 + root 登录 + k8s system:masters）"
  kubectl -n "$NAMESPACE" exec -i "$pod" -- tctl create -f - < "$SCRIPT_DIR/rbac-super-admin.yaml"

  log "把用户 $user 的角色设置为 super-admin（会覆盖原有角色）"
  kubectl -n "$NAMESPACE" exec "$pod" -- tctl users update "$user" --set-roles=super-admin
  kubectl -n "$NAMESPACE" exec "$pod" -- tctl get user/"$user"
  echo
  log "已生效：用户需要重新 tsh login / 重新打开 Web UI 才会拿到新权限"
}

do_status() {
  require_cmds
  helm -n "$NAMESPACE" status "$RELEASE" 2>/dev/null | head -20 || warn "release 不存在"
  echo
  kubectl -n "$NAMESPACE" get pods,svc,certificate 2>/dev/null || true
}

do_uninstall() {
  require_cmds
  helm -n "$NAMESPACE" uninstall "$RELEASE"
  warn "PVC 和命名空间保留；需要彻底清理再手动删除"
}

case "${1:-install}" in
  install)    do_install ;;
  verify)     do_verify ;;
  autoupdate) do_autoupdate ;;
  admin)      do_admin ;;
  rbac)       do_rbac "${2:-}" ;;
  status)     do_status ;;
  uninstall)  do_uninstall ;;
  -h|--help|help) sed -n '2,25p' "$0" ;;
  *) die "未知子命令: $1" ;;
esac
