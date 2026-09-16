#!/usr/bin/env bash
# Regenerate the test fixtures: build the sample RPMs and a createrepo_c
# reference repository that the golden tests compare against.
# Requires: rpmbuild, createrepo_c.
set -euo pipefail
cd "$(dirname "$0")"

rm -rf rpmbuild repo
mkdir -p repo

rpmbuild --define "_topdir $PWD/rpmbuild" --define "dist %{nil}" \
    -bb specs/hello.spec specs/libfoo.spec

cp rpmbuild/RPMS/noarch/hello-2.10-3.noarch.rpm \
   rpmbuild/RPMS/x86_64/libfoo-1.3.0-1.x86_64.rpm repo/

# gzip compression for broad RHEL 8 compatibility (matches the tool default).
createrepo_c --general-compress-type=gz repo

echo "fixtures regenerated under $PWD"
