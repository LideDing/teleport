#!/usr/bin/env bash
# Regenerates tai-oidc/hooks.patch and tai-oidc/tests.patch from the working
# tree of the TAI OIDC branch.
#
# Usage: tai-oidc/gen-patches.sh [BASE_REF]   (default: upstream tag the branch is based on)
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
BASE="${1:-$(cat tai-oidc/BASE 2>/dev/null || echo v18.10.0)}"

mapfile -t HOOKS < <(grep -v '^\s*#' tai-oidc/hook-files.txt | sed '/^\s*$/d')

git diff "$BASE" -- "${HOOKS[@]}" > tai-oidc/hooks.patch
git diff "$BASE" -- lib/web/apiserver_test.go > tai-oidc/tests.patch
echo "$BASE" > tai-oidc/BASE

echo "hooks.patch: $(grep -c '^+[^+]' tai-oidc/hooks.patch) added lines over ${#HOOKS[@]} files"
echo "tests.patch: $(grep -c '^+[^+]' tai-oidc/tests.patch) added lines"
