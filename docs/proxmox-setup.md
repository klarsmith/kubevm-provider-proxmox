# Proxmox VE setup

What the provider needs on the Proxmox side. Verified end to end on PVE
9.2.9 arm64 and PVE 9.2.2 amd64 (see
[findings.md](findings.md#real-pve-smoke-test-2026-10-05)).
`hack/pve-dev-setup.sh` does all of this on a throwaway PVE; read it
alongside this page.

## 1. Pool

```sh
pveum pool add kubevm-dev
```

The manager only creates, changes or deletes VMs in the pool named by its
`--allowed-pool` flag (default `kubevm-dev`). VMs outside the pool are never
touched, so use a dedicated pool.

## 2. Template

Any cloud image with cloud-init works. The template needs:

- **a DNS-style name** (`debian-12`, not `Debian_12`). The name goes
  through a Kubernetes `ObjectReference.name`, which only accepts
  lowercase letters, digits, `-` and `.`.
- **a cloud-init drive**: `ide2` on x86, or `scsi1` on arm64, whose
  `virt` machine has no IDE bus.
- **`agent: 1`** and **`qemu-guest-agent` installed** in the image.
  Without it the VM runs, but reports no addresses. Debian's and Ubuntu's
  cloud images do not ship the agent; install it into the image, or via
  cloud-init vendor data as the setup script does.
- on arm64: `bios: ovmf` and an EFI disk.

Debian 12 on x86, by hand:

```sh
wget https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2
qm create 9000 --name debian-12 --ostype l26 --memory 1024 --cores 1 \
  --net0 virtio,bridge=vmbr0 --scsihw virtio-scsi-single --agent enabled=1 \
  --serial0 socket --vga serial0
qm set 9000 --scsi0 local-lvm:0,import-from=$PWD/debian-12-genericcloud-amd64.qcow2
qm set 9000 --ide2 local-lvm:cloudinit --boot order=scsi0 --ciuser debian --ipconfig0 ip=dhcp
qm template 9000
```

Then get the guest agent in, either baked into the image, or with a
vendor-data snippet (see `hack/pve-dev-setup.sh`, which writes
`local:snippets/kubevm-vendor.yaml` and sets `--cicustom vendor=...`).

The template may live outside the pool. Cloning reads it; nothing writes
to it.

## 3. User, role and token

A dedicated user with a least-privilege role, granted on exactly the paths
the provider touches:

```sh
pveum role add KubeVMProvider --privs "VM.Allocate VM.Clone \
  VM.Config.Disk VM.Config.CPU VM.Config.Memory VM.Config.Network \
  VM.Config.Options VM.Config.Cloudinit VM.PowerMgmt VM.Audit \
  VM.GuestAgent.Audit Datastore.AllocateSpace Datastore.Audit \
  Pool.Allocate Pool.Audit SDN.Use Sys.Audit"
pveum user add kubevm@pve
for path in /pool/kubevm-dev /vms/9000 /storage/local-lvm /sdn/zones/localnetwork/vmbr0 /nodes; do
  pveum acl modify "$path" --users kubevm@pve --roles KubeVMProvider
done
pveum acl modify / --users kubevm@pve --roles PVEAuditor --propagate 0
pveum user token add kubevm@pve ctl --privsep 0
```

Notes:

- **PVE 9 removed `VM.Monitor`.** Guest-agent reads need
  `VM.GuestAgent.Audit`. On PVE 8, use `VM.Monitor` instead.
- `/vms/<template id>` lets the token see and clone the template.
- If the template uses a cloud-init snippet (`cicustom`), also grant the
  role on the storage holding it; `hack/pve-dev-setup.sh` grants
  `/storage/local` for exactly this reason.
- `/sdn/zones/localnetwork/<bridge>` grants `SDN.Use` on the bridge
  (PVE 8+). Add one entry per bridge.
- `PVEAuditor` on `/` (not propagated) is for `/cluster/status`, which
  gives the cluster name for `providerID`.
- **A VM being cloned is in no pool until the clone finishes.** A
  pool-scoped token cannot see it during that time. The provider is built
  for this (it reserves the VMID before cloning); do not "fix" it by
  granting the token wider rights.

## 4. Credentials Secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: pve-credentials
  namespace: <where your machines live>
stringData:
  url: https://pve.example.com:8006
  tokenID: kubevm@pve!ctl
  tokenSecret: <secret printed by "pveum user token add">
  # caBundle: |
  #   -----BEGIN CERTIFICATE-----
  # insecureSkipVerify: "true"   # self-signed test setups only
```

Each `ProxmoxMachine` names it in `spec.credentialsSecretRef`.
