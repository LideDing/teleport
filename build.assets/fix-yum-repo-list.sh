#!/bin/sh
# CentOS 7 is EOL: mirror.centos.org / mirrorlist are gone and vault.centos.org
# may be unreachable. Point all CentOS 7 / SCLo / EPEL repos at the Tencent
# mirror. Needs to be re-run after installing *-release packages, as they ship
# repo files with the old URLs.
set -e
M=https://mirrors.tencent.com
V=7.9.2009
sed -e 's|^mirrorlist=|#mirrorlist=|' \
    -e 's|^metalink=|#metalink=|' \
    -e "s|^#* *baseurl=.*/centos/\$releasever/|baseurl=$M/centos-vault/$V/|" \
    -e "s|^#* *baseurl=.*/centos/7/|baseurl=$M/centos-vault/$V/|" \
    -e "s|^#* *baseurl=.*/altarch/\$releasever/|baseurl=$M/centos-vault/altarch/$V/|" \
    -e "s|^#* *baseurl=.*/altarch/7/|baseurl=$M/centos-vault/altarch/$V/|" \
    -e "s|^#* *baseurl=.*/pub/epel/7/|baseurl=$M/epel-archive/7/|" \
    -i /etc/yum.repos.d/*.repo
if [ "$(uname -m)" = 'aarch64' ]; then
    sed "s|centos-vault/$V/sclo|centos-vault/altarch/$V/sclo|" -i /etc/yum.repos.d/*.repo
fi
