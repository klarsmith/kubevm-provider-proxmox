#!/usr/bin/env bash
# Runs a local test PVE in QEMU on an Apple Silicon Mac. First run installs
# from pve-auto.iso; later runs boot the installed disk. The serial console
# is appended to serial.log in the VM's directory.
#
#   PVE_ARCH=arm64 (default): HVF-accelerated, API on https://127.0.0.1:8006,
#                             SSH on 127.0.0.1:2222.
#   PVE_ARCH=amd64:           x86 under TCG (full emulation; expect hours),
#                             API on https://127.0.0.1:8007, SSH on :2223.
#
# Usage: hack/mac-pve/run.sh   (via `make mac-pve-run [PVE_ARCH=amd64]`; foreground)
# Stop it cleanly with: ssh -p <SSH port> root@127.0.0.1 poweroff
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
QEMU_SHARE=$(brew --prefix qemu)/share/qemu
PVE_ARCH=${PVE_ARCH:-arm64}
MEMORY=${MEMORY:-8192}

case ${PVE_ARCH} in
  arm64) DIR=${ROOT}/.local/pve-vm API_PORT=8006 SSH_PORT=2222 CPUS=${CPUS:-4} ;;
  amd64) DIR=${ROOT}/.local/pve-vm-amd64 API_PORT=8007 SSH_PORT=2223 CPUS=${CPUS:-6} ;;
  *) echo "PVE_ARCH must be arm64 or amd64" >&2; exit 1 ;;
esac

cd "${DIR}"
[[ -f disk.qcow2 ]] || qemu-img create -f qcow2 disk.qcow2 64G

installing=false
if [[ ! -f installed ]]; then
  [[ -f pve-auto.iso ]] || { echo "missing pve-auto.iso; run make mac-pve-iso" >&2; exit 1; }
  installing=true
fi

# shellcheck disable=SC2054 # commas are QEMU option syntax
common=(
  -name "pve-dev-${PVE_ARCH}" -smp "${CPUS}" -m "${MEMORY}"
  -drive if=virtio,format=qcow2,file=disk.qcow2,cache=writeback,discard=unmap
  -netdev "user,id=n0,hostfwd=tcp:127.0.0.1:${API_PORT}-:8006,hostfwd=tcp:127.0.0.1:${SSH_PORT}-:22"
  -device virtio-net-pci,netdev=n0
  -device virtio-rng-pci
  -serial "file:${DIR}/serial.log" -display none -monitor none
)
if ${installing}; then
  # The installer powers off or reboots when done; -no-reboot makes both exit.
  common+=(-no-reboot)
fi

cdrom=()
if [[ ${PVE_ARCH} == arm64 ]]; then
  [[ -f efivars.fd ]] || cp "${QEMU_SHARE}/edk2-arm-vars.fd" efivars.fd
  if ${installing}; then
    # shellcheck disable=SC2054 # commas are QEMU option syntax
    cdrom=(-drive if=none,id=cd,media=cdrom,readonly=on,file=pve-auto.iso
      -device scsi-cd,bus=scsi0.0,drive=cd,bootindex=0)
  fi
  qemu-system-aarch64 -machine virt,highmem=on -accel hvf -cpu host \
    -drive "if=pflash,format=raw,readonly=on,file=${QEMU_SHARE}/edk2-aarch64-code.fd" \
    -drive if=pflash,format=raw,file=efivars.fd \
    -device virtio-scsi-pci,id=scsi0 \
    ${cdrom[@]+"${cdrom[@]}"} "${common[@]}"
else
  # SeaBIOS (legacy boot), so the x86 run also covers the non-UEFI template
  # path. -cpu max exposes the x86-64-v2+ features PVE 9 expects. TCG does
  # not emulate VMX, so this PVE runs its own guests with kvm=0.
  if ${installing}; then
    cdrom=(-cdrom pve-auto.iso -boot order=dc)
  fi
  qemu-system-x86_64 -machine q35 -accel tcg,thread=multi,tb-size=1024 -cpu max \
    ${cdrom[@]+"${cdrom[@]}"} "${common[@]}"
fi

# A clean exit of the install run means the disk now holds PVE.
if ${installing}; then
  touch installed
  echo "install finished; run again to boot the installed PVE"
fi
