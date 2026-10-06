#!/usr/bin/env bash
# Prepares a THROWAWAY Proxmox VE (e.g. a nested PVE VM) for testing
# kubevm-provider-proxmox. Run as root on that PVE's shell. It:
#   1. adds an internal NAT bridge (vmbr1, 10.99.0.0/24) with DHCP via
#      dnsmasq, so guests get addresses no matter what the outer network
#      allows (e.g. hosting providers that filter unknown MACs);
#   2. builds a Debian 12 cloud-init template ("debian-12", VMID 9000) whose
#      vendor-data installs qemu-guest-agent on first boot;
#   3. creates pool "kubevm-dev", a least-privilege role, user kubevm@pve
#      and an API token, and prints a credentials Secret for the provider.
#
# Refuses to run on a node that already has VMs: it rewrites apt sources
# and /etc/network/interfaces, which is not something to do to a real host.
set -euo pipefail

POOL=${POOL:-kubevm-dev}
STORAGE=${STORAGE:-local-lvm}
TEMPLATE_ID=${TEMPLATE_ID:-9000}
TEMPLATE_NAME=${TEMPLATE_NAME:-debian-12}
BRIDGE=${BRIDGE:-vmbr1}
NET=${NET:-10.99.0}
UPLINK=${UPLINK:-vmbr0}
USER_ID=kubevm@pve
TOKEN_NAME=ctl
ROLE=KubeVMProvider
# API_URL is how the provider will reach this PVE; default is its uplink IP.
API_URL=${API_URL:-}
ARCH=$(dpkg --print-architecture) # amd64, or arm64 for PVE on Arm
IMG_URL=https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-${ARCH}.qcow2
IMG=/var/lib/vz/template/debian-12-genericcloud-${ARCH}.qcow2
SNIPPET=/var/lib/vz/snippets/kubevm-vendor.yaml

log() { printf '\n==> %s\n' "$*" >&2; }

# stdout carries only the credentials Secret, so it can be redirected
# straight into a file; everything else (qm, apt, pvesm output) goes to
# stderr.
exec 3>&1 1>&2

[[ ${EUID} -eq 0 ]] || { echo "run as root" >&2; exit 1; }
command -v qm >/dev/null || { echo "not a Proxmox VE host" >&2; exit 1; }
if qm list | awk -v t="${TEMPLATE_ID}" 'NR>1 && $1!=t' | grep -q .; then
  echo "refusing: this node already has VMs. Run this on a throwaway PVE only." >&2
  exit 1
fi

log "apt: switch to the no-subscription repository"
. /etc/os-release
for f in /etc/apt/sources.list.d/pve-enterprise.list /etc/apt/sources.list.d/ceph.list; do
  [[ -f ${f} ]] && sed -i 's/^deb /# deb /' "${f}"
done
for f in /etc/apt/sources.list.d/pve-enterprise.sources /etc/apt/sources.list.d/ceph.sources; do
  [[ -f ${f} ]] && ! grep -q '^Enabled: false' "${f}" && echo 'Enabled: false' >>"${f}"
done
echo "deb http://download.proxmox.com/debian/pve ${VERSION_CODENAME} pve-no-subscription" \
  >/etc/apt/sources.list.d/pve-no-subscription.list
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq dnsmasq >/dev/null
systemctl disable --now dnsmasq >/dev/null 2>&1 || true # PVE SDN may also use it; we run our own unit

log "network: NAT bridge ${BRIDGE} (${NET}.0/24) with DHCP"
# Guests inherit the uplink's MTU: behind e.g. a Hetzner vSwitch (1400), a
# 1500-byte guest stalls on larger downloads.
uplink_mtu=$(cat "/sys/class/net/${UPLINK}/mtu")
if ! grep -q "iface ${BRIDGE} " /etc/network/interfaces; then
  cat >>/etc/network/interfaces <<EOF

auto ${BRIDGE}
iface ${BRIDGE} inet static
	address ${NET}.1/24
	mtu ${uplink_mtu}
	bridge-ports none
	bridge-stp off
	bridge-fd 0
	post-up   echo 1 > /proc/sys/net/ipv4/ip_forward
	post-up   iptables -t nat -A POSTROUTING -s '${NET}.0/24' -o ${UPLINK} -j MASQUERADE
	post-down iptables -t nat -D POSTROUTING -s '${NET}.0/24' -o ${UPLINK} -j MASQUERADE
EOF
  ifreload -a
fi
cat >/etc/dnsmasq-kubevm.conf <<EOF
interface=${BRIDGE}
bind-interfaces
except-interface=lo
dhcp-range=${NET}.100,${NET}.200,12h
dhcp-option=option:router,${NET}.1
dhcp-option=option:mtu,${uplink_mtu}
# This dnsmasq also answers DNS on the bridge, forwarding to the host's own
# resolvers, so guests resolve wherever the host can (no public DNS needed).
dhcp-option=option:dns-server,${NET}.1
EOF
cat >/etc/systemd/system/dnsmasq-kubevm.service <<EOF
[Unit]
Description=DHCP for ${BRIDGE} (kubevm test guests)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/sbin/dnsmasq --keep-in-foreground --conf-file=/etc/dnsmasq-kubevm.conf
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now dnsmasq-kubevm

log "storage: allow snippets on 'local'"
content=$(pvesh get /storage/local --output-format json | python3 -c 'import json,sys; print(json.load(sys.stdin)["content"])')
[[ ${content} == *snippets* ]] || pvesm set local --content "${content},snippets"
mkdir -p "$(dirname "${SNIPPET}")"
cat >"${SNIPPET}" <<'EOF'
#cloud-config
# Vendor data shared by every clone: the provider reads addresses from the
# QEMU guest agent, which Debian's cloud image does not ship.
packages: [qemu-guest-agent]
runcmd:
  - [systemctl, enable, --now, qemu-guest-agent]
EOF

log "template: ${TEMPLATE_NAME} (${TEMPLATE_ID})"
if ! qm status "${TEMPLATE_ID}" >/dev/null 2>&1; then
  [[ -f ${IMG} ]] || wget -q -O "${IMG}" "${IMG_URL}"
  qm create "${TEMPLATE_ID}" --name "${TEMPLATE_NAME}" --ostype l26 --memory 1024 --cores 1 \
    --net0 "virtio,bridge=${BRIDGE}" --scsihw virtio-scsi-single --agent enabled=1 \
    --serial0 socket --vga serial0
  qm set "${TEMPLATE_ID}" --scsi0 "${STORAGE}:0,import-from=${IMG}"
  ci_drive=ide2
  if [[ ${ARCH} == arm64 ]]; then
    # Arm VMs boot UEFI only and the virt machine has no IDE bus.
    qm set "${TEMPLATE_ID}" --bios ovmf --efidisk0 "${STORAGE}:1,efitype=4m,pre-enrolled-keys=0"
    ci_drive=scsi1
  fi
  qm set "${TEMPLATE_ID}" "--${ci_drive}" "${STORAGE}:cloudinit" --boot order=scsi0 \
    --ciuser debian --ipconfig0 ip=dhcp --cicustom "vendor=local:snippets/$(basename "${SNIPPET}")"
  # Guests run emulated (kvm=0) when this PVE has no usable KVM: nested
  # virtualization off on the outer host (no /dev/kvm), or this PVE itself
  # runs under full emulation (QEMU TCG reports "qemu", not "kvm"). Under
  # TCG, /dev/kvm can exist (emulated SVM) but cannot provide the CPU
  # features PVE's default guest CPU asks for, and every start fails with
  # "Host doesn't support requested features".
  if [[ ! -e /dev/kvm || $(systemd-detect-virt 2>/dev/null || true) == qemu ]]; then
    log "no usable KVM: guests will run emulated (kvm=0)"
    qm set "${TEMPLATE_ID}" --kvm 0
  fi
  qm template "${TEMPLATE_ID}"
fi

log "access: pool ${POOL}, role ${ROLE}, user ${USER_ID}, token ${TOKEN_NAME}"
pveum pool add "${POOL}" 2>/dev/null || true
privs="VM.Allocate VM.Clone VM.Config.Disk VM.Config.CPU VM.Config.Memory VM.Config.Network VM.Config.Options VM.Config.Cloudinit VM.PowerMgmt VM.Audit Datastore.AllocateSpace Datastore.Audit Pool.Allocate Pool.Audit SDN.Use Sys.Audit"
# PVE 9 replaced VM.Monitor with VM.GuestAgent.Audit for agent reads.
if pveum role add "${ROLE}" --privs "${privs} VM.GuestAgent.Audit" 2>/dev/null; then
  agent_priv=VM.GuestAgent.Audit
elif pveum role add "${ROLE}" --privs "${privs} VM.Monitor" 2>/dev/null; then
  agent_priv=VM.Monitor
else
  agent_priv=existing
fi
pveum user add "${USER_ID}" --comment "kubevm-provider-proxmox test" 2>/dev/null || true
for path in "/pool/${POOL}" "/vms/${TEMPLATE_ID}" "/storage/${STORAGE}" /storage/local \
  "/sdn/zones/localnetwork/${BRIDGE}" /nodes; do
  pveum acl modify "${path}" --users "${USER_ID}" --roles "${ROLE}"
done
pveum acl modify / --users "${USER_ID}" --roles PVEAuditor --propagate 0

pveum user token remove "${USER_ID}" "${TOKEN_NAME}" 2>/dev/null || true
secret=$(pveum user token add "${USER_ID}" "${TOKEN_NAME}" --privsep 0 --output-format json |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["value"])')

if [[ -z ${API_URL} ]]; then
  API_URL="https://$(ip -4 -o addr show "${UPLINK}" | awk '{print $4}' | cut -d/ -f1 | head -1):8006"
fi
log "done (guest-agent privilege: ${agent_priv}, PVE $(pveversion | cut -d/ -f2))"
cat >&3 <<EOF
# Save as kubevm-provider-proxmox/.local/credentials.yaml (git-ignored).
apiVersion: v1
kind: Secret
metadata:
  name: pve-credentials
  namespace: team-a
stringData:
  url: ${API_URL}
  tokenID: ${USER_ID}!${TOKEN_NAME}
  tokenSecret: ${secret}
  insecureSkipVerify: "true"   # throwaway PVE with its self-signed certificate
EOF
