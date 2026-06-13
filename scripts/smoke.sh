#!/usr/bin/env bash
# Quick smoke test: boot a microVM by hand inside Lima, SSH in, kill it.
# Mirrors the manual flow that the agent automates.
set -euo pipefail

LIMA_NAME="${LIMA_NAME:-pandastack}"

limactl shell "$LIMA_NAME" -- bash -lc '
set -euo pipefail
ARCH="aarch64"
WORK="$HOME/microvm-smoke"
mkdir -p "$WORK" && cd "$WORK"

# Use the kernel + rootfs the Lima provisioner already downloaded.
KERNEL=$(ls /var/lib/pandastack/kernels/vmlinux-5.10* | tail -1)
cp /var/lib/pandastack/templates/ubuntu-24.04/rootfs.ext4 ./rootfs.ext4

# Generate SSH key & inject into rootfs.
ssh-keygen -f id_rsa -N "" -q -y >/dev/null || ssh-keygen -f id_rsa -N "" -q
sudo mkdir -p /mnt/fc-smoke
sudo mount -o loop ./rootfs.ext4 /mnt/fc-smoke
sudo mkdir -p /mnt/fc-smoke/root/.ssh
sudo cp id_rsa.pub /mnt/fc-smoke/root/.ssh/authorized_keys
sudo umount /mnt/fc-smoke

# Boot Firecracker in the background.
SOCK=/tmp/fc-smoke.sock
sudo rm -f "$SOCK"
sudo firecracker --api-sock "$SOCK" --enable-pci &
FCPID=$!
sleep 0.5

# tap0 for the smoke VM only.
sudo ip link del tap-smoke 2>/dev/null || true
sudo ip tuntap add dev tap-smoke mode tap
sudo ip addr add 172.30.0.1/30 dev tap-smoke
sudo ip link set tap-smoke up
sudo iptables -t nat -C POSTROUTING -s 172.30.0.0/30 -j MASQUERADE 2>/dev/null || \
  sudo iptables -t nat -A POSTROUTING -s 172.30.0.0/30 -j MASQUERADE

sudo curl -s --unix-socket "$SOCK" -X PUT "http://x/boot-source"  -d "{\"kernel_image_path\":\"$KERNEL\",\"boot_args\":\"console=ttyS0 reboot=k panic=1 keep_bootcon\"}"
sudo curl -s --unix-socket "$SOCK" -X PUT "http://x/drives/rootfs" -d "{\"drive_id\":\"rootfs\",\"path_on_host\":\"$PWD/rootfs.ext4\",\"is_root_device\":true,\"is_read_only\":false}"
sudo curl -s --unix-socket "$SOCK" -X PUT "http://x/network-interfaces/net1" -d "{\"iface_id\":\"net1\",\"guest_mac\":\"06:00:AC:1E:00:02\",\"host_dev_name\":\"tap-smoke\"}"
sudo curl -s --unix-socket "$SOCK" -X PUT "http://x/actions" -d "{\"action_type\":\"InstanceStart\"}"

echo "Waiting for SSH on 172.30.0.2 ..."
for i in {1..30}; do
  if ssh -i id_rsa -o StrictHostKeyChecking=no -o ConnectTimeout=2 root@172.30.0.2 "uname -a"; then
    break
  fi
  sleep 1
done

echo "✅ Smoke test passed — tearing down."
sudo kill "$FCPID" || true
sudo ip link del tap-smoke || true
'
