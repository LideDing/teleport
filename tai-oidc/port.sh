#!/usr/bin/env bash
# Ports the TAI OIDC implementation onto the current Teleport checkout
# (typically a fresh branch created from a newer upstream tag).
#
#   SRC is where the implementation lives:
#     - a git ref (e.g. feature/tai-oidc-login), or
#     - a directory containing a checkout of that branch.
#
# Usage (from the TARGET repository root):
#   git show feature/tai-oidc-login:tai-oidc/port.sh > /tmp/port.sh
#   bash /tmp/port.sh feature/tai-oidc-login
#
# Steps: check upstream anchors -> copy new files -> apply hooks.patch with
# 3-way merge -> promote go-oidc to a direct dependency -> gofmt -> build.
# Test expectation updates (tests.patch) are applied best-effort.
set -euo pipefail

SRC="${1:?usage: port.sh <git-ref|source-dir>}"
cd "$(git rev-parse --show-toplevel)"

if [ -d "$SRC" ]; then
	mode=dir
	fetch() { cat "$SRC/$1"; }
else
	git rev-parse --verify --quiet "$SRC^{commit}" >/dev/null || { echo "unknown ref or dir: $SRC" >&2; exit 2; }
	mode=ref
	fetch() { git show "$SRC:$1"; }
fi

KIT=$(mktemp -d)
trap 'rm -rf "$KIT"' EXIT
for f in files.txt hook-files.txt hooks.patch tests.patch check-upstream.sh; do
	fetch "tai-oidc/$f" > "$KIT/$f"
done

echo "== 1/6 Checking upstream anchors =="
rc=0
bash "$KIT/check-upstream.sh" . || rc=$?
case $rc in
0) ;;
3) echo "Target tree is already ported; nothing to do." >&2; exit 0 ;;
*) echo "Upstream anchors changed. Fix the FAIL items (see TAI_OIDC_IMPLEMENTATION.md §5) and re-run." >&2; exit 1 ;;
esac

echo "== 2/6 Copying new files from $SRC ($mode) =="
mapfile -t FILES < <(grep -v '^\s*#' "$KIT/files.txt" | sed '/^\s*$/d')
for f in "${FILES[@]}"; do
	mkdir -p "$(dirname "$f")"
	fetch "$f" > "$f"
	echo "  + $f"
done
# Keep the kit and the docs with the port so it can be ported again later.
mkdir -p tai-oidc
for f in files.txt hook-files.txt hooks.patch tests.patch check-upstream.sh port.sh gen-patches.sh BASE; do
	fetch "tai-oidc/$f" > "tai-oidc/$f" 2>/dev/null || true
done
for f in TAI_OIDC_DESIGN.md TAI_OIDC_IMPLEMENTATION.md; do
	fetch "$f" > "$f" 2>/dev/null || true
done

echo "== 3/6 Applying hooks.patch (3-way) =="
if ! git apply --3way --whitespace=nowarn "$KIT/hooks.patch"; then
	echo "hooks.patch did not apply cleanly; resolve conflicts (git status / git diff) using TAI_OIDC_IMPLEMENTATION.md §4." >&2
	exit 1
fi

echo "== 4/6 Promoting go-oidc to a direct dependency =="
sed -i -E 's#^(\s*github\.com/coreos/go-oidc/v3 v[^ ]+) // indirect$#\1#' go.mod
grep -n 'github.com/coreos/go-oidc/v3' go.mod

echo "== 5/6 gofmt + build =="
gofmt -l lib/oidcsso lib/auth lib/web lib/modules tool/tctl/sso | sed 's/^/  needs gofmt: /' || true
go build ./lib/oidcsso/... ./lib/auth/ ./lib/web/ ./lib/modules/ ./tool/tctl/...

echo "== 6/6 Test expectations (tests.patch, best effort) =="
if git apply --3way --whitespace=nowarn "$KIT/tests.patch"; then
	echo "  applied"
else
	echo "  tests.patch did not apply; update TestGetWebConfig_WithEntitlements by hand (§6.3)"
fi

cat <<'EOF'

Port applied. Next:
  go test ./lib/oidcsso/... -count=1
  go test ./lib/auth/ -run 'TestOIDC' -count=1
  go test ./lib/web/ -run 'TestOIDC|TestGetWebConfig' -count=1
  go test ./lib/modules/ ./tool/tctl/sso/... -count=1
Then follow TAI_OIDC_IMPLEMENTATION.md §6-§7.
EOF
