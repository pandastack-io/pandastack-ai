#!/bin/bash
# Build the PandaStack guest kernel: kernel.org
# linux-$V + Firecracker's 6.1 CI config + ps-6.1.fragment, in a throwaway
# ubuntu:24.04 container. Needs Docker and network; ~5 min on 8 cores.
#
# Output in $W/out: vmlinux-ps-$V.$BUILD (debug info stripped, .BTF kept),
# config-ps-$V.$BUILD and SHA256SUMS. Ship with the steps in README.md.
#
# Usage: V=6.1.192 BUILD=1 kernel/guest-6.1/build.sh
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
V=${V:-6.1.192}
BUILD=${BUILD:-1}
W=${W:-$HERE/work}
mkdir -p "$W/out"
cp "$HERE/firecracker-ci-6.1.188.config" "$W/fc-6.1.config"
cp "$HERE/ps-6.1.fragment" "$W/ps.fragment"
echo "build linux-$V as vmlinux-ps-$V.$BUILD in $W"
docker run --rm -v "$W":/w -w /w -e V="$V" -e BUILD="$BUILD" ubuntu:24.04 bash -euc '
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq && apt-get install -y -qq build-essential flex bison libelf-dev libssl-dev dwarves bc cpio rsync xz-utils curl ca-certificates python3 >/dev/null 2>&1
  echo "toolchain: $(gcc --version | head -1); pahole $(pahole --version)"
  [ -f linux-$V.tar.xz ] || curl -fsSLO https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$V.tar.xz
  rm -rf linux-$V && tar -xf linux-$V.tar.xz && cd linux-$V
  cp ../fc-6.1.config .config
  scripts/kconfig/merge_config.sh -m .config ../ps.fragment >/dev/null
  make olddefconfig >/dev/null
  # We ship no modules, so nothing may be built as one: turn any =m into =y.
  if grep -qE "=m$" .config; then sed -i -E "s/=m$/=y/" .config; make olddefconfig >/dev/null; fi
  [ "$(grep -cE "=m$" .config)" = 0 ] || { echo "ABORT: options still built as modules"; exit 1; }
  bad=0
  while read -r l; do o=${l%%=*}; got=$(grep -E "^$o=" .config || echo "$o off"); if [ "$got" = "$l" ]; then echo "ok   $l"; else echo "DIFF want $l got $got"; bad=1; fi; done < <(grep -vE "^#|^$" ../ps.fragment)
  [ $bad = 0 ] || { echo "ABORT: the fragment did not apply"; exit 1; }
  make -j"$(nproc)" vmlinux > ../build.log 2>&1 || { tail -40 ../build.log; exit 1; }
  cp .config ../out/config-ps-$V.$BUILD
  objcopy --strip-debug vmlinux ../out/vmlinux-ps-$V.$BUILD
  readelf -S ../out/vmlinux-ps-$V.$BUILD | grep -qE "\.BTF " || { echo "ABORT: no .BTF section"; exit 1; }
  cd ../out && sha256sum vmlinux-ps-$V.$BUILD > SHA256SUMS && cat SHA256SUMS && ls -la
'
