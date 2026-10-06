#!/usr/bin/env bash
# Builds an unattended Proxmox VE ISO for a throwaway test PVE. The ISO
# installs PVE to the VM's virtio disk, then on first boot runs
# hack/pve-dev-setup.sh, which prints the provider's credentials Secret to
# the serial console and /root/kubevm-firstboot.log.
#
#   PVE_ARCH=arm64 (default)  QEMU/HVF on an Apple Silicon Mac
#   PVE_ARCH=amd64            QEMU/TCG on a Mac (fully emulated; slow)
#   PVE_ARCH=nested           x86 VM on another Proxmox (real nested KVM);
#                             needs STATIC_CIDR, STATIC_GW, STATIC_DNS
#
# Usage: hack/mac-pve/build-iso.sh   (via `make mac-pve-iso [PVE_ARCH=...]`)
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
PVE_ARCH=${PVE_ARCH:-arm64}
case ${PVE_ARCH} in
  arm64) DIR=${ROOT}/.local/pve-vm ISO=${ISO:-proxmox-ve_9.2-1-arm64.iso} API_PORT=8006 TTY=/dev/ttyAMA0 ;;
  amd64) DIR=${ROOT}/.local/pve-vm-amd64 ISO=${ISO:-proxmox-ve_9.2-1.iso} API_PORT=8007 TTY=/dev/ttyS0 ;;
  nested) DIR=${ROOT}/.local/pve-nested ISO=${ISO:-proxmox-ve_9.2-1.iso} TTY=/dev/ttyS0 ;;
  *) echo "PVE_ARCH must be arm64, amd64 or nested" >&2; exit 1 ;;
esac

API_URL=https://127.0.0.1:${API_PORT:-8006}
network='[network]
# QEMU user networking: 10.0.2.15/24 via DHCP, gateway 10.0.2.2.
source = "from-dhcp"'
if [[ ${PVE_ARCH} == nested ]]; then
  : "${STATIC_CIDR:?set STATIC_CIDR, e.g. 10.25.0.104/24}" "${STATIC_GW:?set STATIC_GW}" "${STATIC_DNS:?set STATIC_DNS}"
  API_URL=https://${STATIC_CIDR%/*}:8006
  network="[network]
source = \"from-answer\"
cidr = \"${STATIC_CIDR}\"
gateway = \"${STATIC_GW}\"
dns = \"${STATIC_DNS}\"
filter.ID_NET_NAME = \"*\""
fi
mkdir -p "${DIR}"

[[ -f ${DIR}/${ISO} ]] || { echo "missing ${DIR}/${ISO}; download it first" >&2; exit 1; }

# Root password: generated once, kept git-ignored next to the VM.
if [[ ! -f ${DIR}/root-password ]]; then
  openssl rand -base64 18 | tr -d '/+=' >"${DIR}/root-password"
  chmod 600 "${DIR}/root-password"
fi
root_password=$(cat "${DIR}/root-password")

ssh_key=""
for f in "${HOME}"/.ssh/id_ed25519.pub "${HOME}"/.ssh/id_ecdsa.pub "${HOME}"/.ssh/id_rsa.pub; do
  if [[ -f ${f} ]]; then
    ssh_key=$(cat "${f}")
    break
  fi
done
[[ -n ${ssh_key} ]] || { echo "no SSH public key in ~/.ssh" >&2; exit 1; }

# The template's [network] block is replaced wholesale for a static setup.
NETWORK_BLOCK=${network} awk '
  /^\[network\]/ { print ENVIRON["NETWORK_BLOCK"]; skip = 1; next }
  /^\[/ { skip = 0 }
  !skip
' "${HERE}/answer.toml.in" |
  sed -e "s|@ROOT_PASSWORD@|${root_password}|" -e "s|@SSH_KEY@|${ssh_key}|" \
    -e "s|pve-dev.kubevm.test|pve-dev-${PVE_ARCH}.kubevm.test|" >"${DIR}/answer.toml"
chmod 600 "${DIR}/answer.toml"

# The first-boot hook is a single script: a header that mirrors all output
# to the serial console, then the setup script itself.
{
  cat <<EOF
#!/usr/bin/env bash
exec > >(tee -a /root/kubevm-firstboot.log ${TTY}) 2>&1
echo "KUBEVM-FIRSTBOOT-BEGIN"
trap 'echo "KUBEVM-FIRSTBOOT-END rc=\$?"' EXIT
# How the provider (on the Mac) reaches this PVE.
export API_URL=${API_URL}
EOF
  sed '1d' "${ROOT}/hack/pve-dev-setup.sh"
} >"${DIR}/firstboot.sh"

docker run --rm --platform linux/arm64 -v "${DIR}:/work" debian:trixie bash -euo pipefail -c '
  apt-get update -qq
  apt-get install -y -qq curl ca-certificates >/dev/null
  curl -fsSL -o /usr/share/keyrings/proxmox-archive-keyring.gpg \
    https://enterprise.proxmox.com/debian/proxmox-archive-keyring-trixie.gpg
  echo "deb [signed-by=/usr/share/keyrings/proxmox-archive-keyring.gpg] http://download.proxmox.com/debian/pve trixie pve-no-subscription" \
    >/etc/apt/sources.list.d/pve.list
  apt-get update -qq
  apt-get install -y -qq proxmox-auto-install-assistant xorriso >/dev/null
  proxmox-auto-install-assistant validate-answer /work/answer.toml
  rm -f /work/pve-auto.iso
  proxmox-auto-install-assistant prepare-iso "/work/'"${ISO}"'" \
    --fetch-from iso --answer-file /work/answer.toml \
    --on-first-boot /work/firstboot.sh --output /work/pve-auto.iso
'
echo "built ${DIR}/pve-auto.iso"
