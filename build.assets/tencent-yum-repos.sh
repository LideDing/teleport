#!/bin/sh
# Replace all CentOS 7 yum repositories with the Tencent software mirrors.
#
# This script is intentionally idempotent: installing epel-release and
# centos-release-scl-rh rewrites /etc/yum.repos.d, so the Dockerfiles invoke it
# again after those packages are installed.
#
# devtoolset-12 is the only repository that cannot be mirrored: those unsigned
# RPMs only exist in CentOS buildlogs (the Tencent vault stops at devtoolset-11).
# DEVTOOLSET_REPO_BASE can be set to an internal mirror of buildlogs.centos.org.
set -eu

case "$(uname -m)" in
    x86_64)  VAULT=centos-vault/7.9.2009 ; DEVTOOLSET_ARCH=x86_64 ;;
    aarch64) VAULT=centos-vault/altarch/7.9.2009 ; DEVTOOLSET_ARCH=aarch64 ;;
    *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

BASE="https://mirrors.tencent.com/${VAULT}"
CENTOS_KEY=https://mirrors.tencent.com/centos-vault/RPM-GPG-KEY-CentOS-7
EPEL_KEY=https://mirrors.tencent.com/epel/RPM-GPG-KEY-EPEL-7
DEVTOOLSET_BASE="${DEVTOOLSET_REPO_BASE:-https://buildlogs.centos.org}"

rm -f /etc/yum.repos.d/*.repo

cat > /etc/yum.repos.d/tencent-centos7.repo <<EOF
[os]
name=Tencent CentOS 7 - os
baseurl=${BASE}/os/\$basearch/
enabled=1
gpgcheck=1
gpgkey=${CENTOS_KEY}

[updates]
name=Tencent CentOS 7 - updates
baseurl=${BASE}/updates/\$basearch/
enabled=1
gpgcheck=1
gpgkey=${CENTOS_KEY}

[extras]
name=Tencent CentOS 7 - extras
baseurl=${BASE}/extras/\$basearch/
enabled=1
gpgcheck=1
gpgkey=${CENTOS_KEY}

[base-source]
name=Tencent CentOS 7 - Base Sources
baseurl=${BASE}/os/Source/
enabled=1
gpgcheck=1
gpgkey=${CENTOS_KEY}

[updates-source]
name=Tencent CentOS 7 - Updates Sources
baseurl=${BASE}/updates/Source/
enabled=1
gpgcheck=1
gpgkey=${CENTOS_KEY}

[sclo-rh]
name=Tencent CentOS 7 - SCLo rh
baseurl=${BASE}/sclo/\$basearch/rh/
enabled=1
gpgcheck=1
gpgkey=${CENTOS_KEY}

[sclo]
name=Tencent CentOS 7 - SCLo sclo
baseurl=${BASE}/sclo/\$basearch/sclo/
enabled=1
gpgcheck=1
gpgkey=${CENTOS_KEY}

[epel]
name=Tencent EPEL 7
baseurl=https://mirrors.tencent.com/epel/7/\$basearch/
enabled=1
gpgcheck=1
gpgkey=${EPEL_KEY}

[${DEVTOOLSET}-build]
name=${DEVTOOLSET} - Build
baseurl=${DEVTOOLSET_BASE}/c7-${DEVTOOLSET}.${DEVTOOLSET_ARCH}/
enabled=1
gpgcheck=0
EOF

# GIT_MIRROR is optional: Tencent does not mirror github.com. Set it to an
# internal git mirror prefix (with trailing slash) to rewrite all clones.
if [ -n "${GIT_MIRROR:-}" ] && command -v git >/dev/null 2>&1; then
    git config --global url."${GIT_MIRROR}".insteadOf "https://github.com/"
fi
