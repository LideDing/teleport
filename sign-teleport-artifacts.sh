#!/usr/bin/env bash
#
# 为自编译的 Teleport OSS 产物生成 .sha256 和 OpenSSL 分离签名 .sig
# 签名格式：base64(ECDSA P-256 + SHA-256 + ASN.1 DER)，与 teleport-update 的校验逻辑一致
#
# 用法：
#   ./sign-teleport-artifacts.sh --keygen                 # 首次：生成密钥对并签名全部产物
#   ./sign-teleport-artifacts.sh                          # 签名 build/artifacts 下全部产物
#   ./sign-teleport-artifacts.sh a.tar.gz b.tar.gz        # 只签名指定文件
#   TELEPORT_SIGN_PASS=xxx ./sign-teleport-artifacts.sh   # 私钥加密（推荐）
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ARTIFACT_DIR="${ARTIFACT_DIR:-$ROOT/build/artifacts}"
KEY="${TELEPORT_SIGN_KEY:-$ROOT/cosign.key}"
PUB="${TELEPORT_SIGN_PUB:-$ROOT/cosign.pub}"

KEYGEN=0
KEYGEN_ONLY=0
FORCE=0
FILES=()

usage() {
  cat <<'USAGE'
为自编译的 Teleport OSS 产物生成 .sha256 和 OpenSSL 分离签名 .sig
签名格式：base64(ECDSA P-256 + SHA-256 + ASN.1 DER)，与 teleport-update 的校验逻辑一致

用法：
  ./sign-teleport-artifacts.sh --keygen                 # 首次：生成密钥对并签名全部产物
  ./sign-teleport-artifacts.sh                          # 签名 build/artifacts 下全部产物
  ./sign-teleport-artifacts.sh a.tar.gz b.tar.gz        # 只签名指定文件
  TELEPORT_SIGN_PASS=xxx ./sign-teleport-artifacts.sh   # 私钥加密（推荐）

参数：
  --keygen        生成 P-256 密钥对（已存在时需配合 --force）
  --keygen-only   只生成密钥对，不签名
  --force         覆盖已存在的私钥
  --key PATH      私钥路径，默认 cosign.key
  --pub PATH      公钥路径，默认 cosign.pub
  --dir PATH      产物目录，默认 build/artifacts
  -h, --help      显示本帮助
USAGE
}

die() { echo "error: $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --keygen)  KEYGEN=1; shift ;;
    --keygen-only) KEYGEN=1; KEYGEN_ONLY=1; shift ;;
    --force)   FORCE=1; shift ;;
    --key)     KEY="${2:?--key 需要路径}"; shift 2 ;;
    --pub)     PUB="${2:?--pub 需要路径}"; shift 2 ;;
    --dir)     ARTIFACT_DIR="${2:?--dir 需要路径}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    -*)        die "未知参数：$1（-h 查看用法）" ;;
    *)         FILES+=("$1"); shift ;;
  esac
done

command -v openssl >/dev/null 2>&1 || die "未找到 openssl"

# 私钥有密码时通过环境变量传入，避免出现在进程参数和 shell history 里
PASSIN_OPT=()
if [[ -n "${TELEPORT_SIGN_PASS:-}" ]]; then
  export TELEPORT_SIGN_PASS
  PASSIN_OPT=(-passin env:TELEPORT_SIGN_PASS)
fi

gen_keypair() {
  if [[ -e "$KEY" && $FORCE -ne 1 ]]; then
    die "$KEY 已存在。重新生成会让已内置旧公钥的 teleport-update 失效，确认请加 --force"
  fi

  local tmp
  tmp="$(mktemp)"
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:prime256v1 -out "$tmp"

  if [[ -n "${TELEPORT_SIGN_PASS:-}" ]]; then
    openssl pkcs8 -topk8 -in "$tmp" -out "$KEY" -v2 aes-256-cbc \
      -passout env:TELEPORT_SIGN_PASS
    rm -f "$tmp"
  else
    mv "$tmp" "$KEY"
  fi

  chmod 600 "$KEY"
  openssl pkey "${PASSIN_OPT[@]}" -in "$KEY" -pubout -out "$PUB"
  base64 -w0 "$PUB" > "$PUB.b64"
  echo "已生成密钥对：$KEY"
  echo "               $PUB"
}

sign_one() {
  local tar="$1" der rc=0
  [[ -f "$tar" ]] || die "文件不存在：$tar"

  # 只写文件名，便于在产物目录直接 sha256sum -c；Teleport 只读前 64 个字符
  ( cd "$(dirname "$tar")" && sha256sum "$(basename "$tar")" ) > "$tar.sha256"

  # base64 -A 保证单行；Teleport 用 base64.StdEncoding 严格解码，不能带换行
  openssl dgst -sha256 "${PASSIN_OPT[@]}" -sign "$KEY" "$tar" \
    | openssl base64 -A > "$tar.sig" || rc=$?
  [[ $rc -eq 0 ]] || die "签名失败：$tar"

  der="$(mktemp)"
  base64 -d "$tar.sig" > "$der" || { rm -f "$der"; die "签名不是合法 base64：$tar.sig"; }
  if ! openssl dgst -sha256 -verify "$PUB" -signature "$der" "$tar" >/dev/null; then
    rm -f "$der"
    die "签名自检失败（公钥与私钥不匹配？）：$tar"
  fi
  rm -f "$der"

  echo "  ok  $(basename "$tar")"
}

[[ $KEYGEN -eq 1 ]] && gen_keypair
[[ $KEYGEN_ONLY -eq 1 ]] && exit 0

[[ -f "$KEY" ]] || die "私钥不存在：$KEY（首次使用请加 --keygen）"
if [[ ! -f "$PUB" ]]; then
  openssl pkey "${PASSIN_OPT[@]}" -in "$KEY" -pubout -out "$PUB"
fi
[[ -f "$PUB.b64" ]] || base64 -w0 "$PUB" > "$PUB.b64"

if [[ ${#FILES[@]} -eq 0 ]]; then
  shopt -s nullglob
  FILES=("$ARTIFACT_DIR"/teleport-v*-bin.tar.gz)
  shopt -u nullglob
  [[ ${#FILES[@]} -gt 0 ]] || die "在 $ARTIFACT_DIR 下没找到 teleport-v*-bin.tar.gz"
fi

echo "开始签名（私钥：$KEY）"
for f in "${FILES[@]}"; do
  sign_one "$f"
done

cat <<EOF

完成。每个产物现在都有 .sha256 和 .sig：
$(printf '  %s\n' "${FILES[@]}")

公钥已写入 $PUB.b64。编译 teleport-update 必须使用同一把公钥，否则下载时会报
"artifact signature verification failed for all trusted keys"：

  KEY=\$(cat $PUB.b64)
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -buildvcs=false \\
    -ldflags "-X main.artifactSignaturePublicKeyB64=\$KEY -X main.artifactSignatureBackupPublicKeyB64=\$KEY" \\
    -o build/teleport-update ./tool/teleport-update
EOF
