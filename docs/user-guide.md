# User guide

How to run virtual machines on Proxmox VE through KubeVM with this provider.
Proxmox-side preparation is in [proxmox-setup.md](proxmox-setup.md).

## The two objects

A machine is two objects in the same namespace:

- a **`VirtualMachine`** (`kube-vm.io/v1alpha1`), the portable description:
  image, size, power state, network, SSH keys. This is what you edit.
- a **`ProxmoxMachine`** (`infrastructure.kube-vm.io/v1alpha1`), the
  Proxmox half: which Proxmox, which pool, which bridge.

They must name each other. The `VirtualMachine`'s `spec.infrastructureRef`
names the `ProxmoxMachine`, and the `ProxmoxMachine` carries the annotation
`kube-vm.io/virtual-machine: <VirtualMachine name>`. One-sided references
are ignored, so nobody can claim a machine they did not create. A machine
stuck in `NotAdopted` almost always has a missing or misspelled annotation.

See [config/samples/virtualmachine.yaml](../config/samples/virtualmachine.yaml)
for a complete pair.

## What maps to what

| `VirtualMachine` field | Proxmox |
|---|---|
| `bootDisk.source.image.name` | name of the template VM to clone (`apiGroup`/`kind` are required by the schema but not resolved) |
| `bootDisk.sizeGiB` | boot disk grown to this size after the clone; never shrunk |
| `bootDisk.deleteOnTermination: false` | not supported yet: deletion is blocked rather than destroying the disk |
| `instanceType.resources.cpus` | `cores` |
| `instanceType.resources.memory` | `memory`, rounded up to MiB |
| `powerState` | start / shutdown / stop, see [Power](#power) |
| `powerOffMode` | `Hard` → stop, `Soft` → shutdown that fails if the guest refuses, `TrySoft` (default) → shutdown, then stop after 180 s |
| `network.hostName` | VM name, which Proxmox cloud-init uses as the hostname |
| `network.nameservers` / `searchDomains` | `nameserver` / `searchdomain` |
| `network.interfaces[N]` | `netN` on `ProxmoxMachine.spec.bridge`, `ipconfigN` (DHCP or static IPv4) |
| `sshPublicKeys` | `sshkeys` for the cloud-init user |

Fields the provider does not implement are reported on
`UpToDate=False/UnsupportedByProvider` with a list, not ignored:
`instanceType.name`, data `disks`, non-image boot sources,
`storageClassName`, `volumeAttributesClassName`, interface `network`
references, `publicIP`, `dhcp6`, static IPv6, `bootstrap.cloudInit`
user/network data, `failureDomain`, `scheduling.spot`, `tags`, and the
`Suspended` power state.

## `ProxmoxMachine` spec

Two kinds of field. **Resolved** fields are copied from the
`VirtualMachine` on every reconcile, so `kubectl get pxm -o yaml` shows what
the controller is acting on. Edit the `VirtualMachine`, not these:
`template`, `powerState`, `powerOffMode`, `cpus`, `memoryMiB`,
`bootDiskSizeGiB`, `deleteOnTermination`, `hostName`, `nameservers`,
`searchDomains`, `sshPublicKeys`, `interfaces`.

**Provider-only** fields have no portable equivalent. You set them once,
and the controller never writes them:

| Field | Default | Meaning |
|---|---|---|
| `credentialsSecretRef.name` | (required) | Secret in the same namespace, see below |
| `pool` | `kubevm-dev` | Proxmox pool for the VM. Must equal the manager's `--allowed-pool` |
| `node` | template's node | node to place the clone on |
| `storage` | template's storage | target storage for a full clone |
| `fullClone` | `true` | full or linked clone |
| `bridge` | `vmbr0` | bridge for every interface in `network.interfaces` |
| `staticIPPrefixLength` | none | prefix length for static addresses (KubeVM addresses are bare IPs) |
| `gateway` | none | IPv4 gateway for static addresses |
| `ciUser` | template's | cloud-init user |

### Credentials Secret

| Key | Required | Example |
|---|---|---|
| `url` | yes | `https://pve.example.com:8006` |
| `tokenID` | yes | `kubevm@pve!ctl` |
| `tokenSecret` | yes | the token's UUID secret |
| `caBundle` | no | PEM CA for the Proxmox certificate |
| `insecureSkipVerify` | no | `"true"` to skip TLS verification (test setups only) |

## Status

| Field | Meaning |
|---|---|
| `powerState` | `PoweredOn` / `PoweredOff`, absent while a power task runs |
| `addresses[]` | from the QEMU guest agent, loopback and link-local dropped; named after the portable interface where the MAC matches |
| `providerID` | `proxmox://<cluster>/<vmid>`; on an unclustered host `<cluster>` is the node name |
| `providerMetadata` | `node`, `vmid`, `pool`, `template`, `pveVersion` |
| `vmid` | the Proxmox VMID, reserved before the clone is sent |
| `pendingTask` / `pendingOp` | UPID and kind of the Proxmox task in flight |
| `provisioned` | pre-boot configuration done |
| `consecutiveFailures`, `lastFailure`, `nextAttempt` | failure streak and when the next task may start |

### Conditions

`InfrastructureReady` and `UpToDate` are what KubeVM mirrors onto the
`VirtualMachine`.

| Reason | Condition | Means |
|---|---|---|
| `Running` / `Stopped` | Ready=True | VM is in a steady power state |
| `Provisioning` | Ready=False | VMID reserved, clone or pre-boot setup in progress |
| `TaskRunning` | Ready=False | a Proxmox task is in flight |
| `PowerChanging` | Ready=False | a power task was just sent |
| `TaskFailed` | Ready=False and/or UpToDate=False | a task failed; message has the Proxmox exit status and the next attempt time |
| `NotAdopted` | Ready=False | the two-sided link is incomplete |
| `InvalidConfiguration` | Ready=False | e.g. no template of that name, static address without prefix length, unreadable Secret |
| `OutsidePool` | Ready=False | `spec.pool` or the VM's actual pool is not the allowed pool; nothing is done |
| `VMGone` | Ready=False | the VM was removed outside Kubernetes; it is not recreated |
| `Paused` | Ready=False | the VM is paused, by a user or by QEMU on an I/O error such as full storage; not acted on |
| `APIError` | Ready=False | a Proxmox API call failed |
| `Deleting` | Ready=False | delete in progress |
| `Applied` | UpToDate=True | the spec is applied |
| `UnsupportedByProvider` | UpToDate=False | some asked-for fields are not implemented |

## Power

Edit `spec.powerState` on the `VirtualMachine`. The provider sends one
Proxmox task, records its UPID, and polls it.

- `Soft` asks the guest (ACPI, or the guest agent if installed) and does
  **not** escalate. A guest that refuses within 180 s fails the task:
  `UpToDate=False/TaskFailed`, and the VM keeps running. Retries follow the
  backoff.
- `TrySoft` asks the guest, and Proxmox stops the VM after 180 s.
- `Hard` stops at once.

After a failed task, no new task starts until `status.nextAttempt`
(1 min, doubling per failure, capped at 30 min). A task that succeeds
clears the streak. So does changing `powerState` or `powerOffMode`: a new
request is not held back by the old one's failures.

The provider waits for VM lifecycle tasks it finds running (start,
shutdown, stop, clone, destroy, resize, migrate, disk move), including
ones started outside Kubernetes. Consoles, backups and other tasks are
ignored. If one holds a lock that makes a power task fail, that failure
backs off like any other.

## Networking

- No `network.interfaces`: the template's NICs and cloud-init network
  settings are left as they are.
- Each interface `N` becomes `netN` on `spec.bridge`. An existing NIC keeps
  its model and MAC; only the bridge changes.
- DHCP: `dhcp4: true`, or no addresses.
- Static IPv4: put a bare address in `addresses` and set
  `staticIPPrefixLength` (and usually `gateway`) on the `ProxmoxMachine`.
  A static address without a prefix length is rejected before anything is
  sent to Proxmox.

Addresses only appear when the QEMU guest agent runs in the guest and
`agent: 1` is set on the template.

## Delete

Deleting the `VirtualMachine` deletes the `ProxmoxMachine`, which:

1. waits for any running task, including an unfinished clone;
2. hard-stops the VM if it is running;
3. destroys it with `purge=1` and `destroy-unreferenced-disks=1`;
4. destroys any other VM in the pool carrying this machine's
   `kubevm-uid=` marker;
5. releases the finalizer.

A VM that has left the allowed pool is not touched; the finalizer is
released anyway.

A failed stop or destroy backs off before the next attempt. A backoff left
by anything else, such as a VM that could not start, does not delay
deletion.

**Keep the credentials Secret until the machines are gone.** Without it the
controller cannot reach Proxmox, and deletion waits, retrying with
backoff until the Secret is back. This matters when
deleting a whole namespace: delete the `VirtualMachine`s first.

## Troubleshooting

| Symptom | Look at |
|---|---|
| Stuck in `NotAdopted` | the `kube-vm.io/virtual-machine` annotation and `spec.infrastructureRef` |
| `OutsidePool` | `spec.pool` vs the manager's `--allowed-pool`; whether someone moved the VM |
| `InvalidConfiguration: no Proxmox template named ...` | the template's name, that it is a template, and that the token can see it (`VM.Audit` on `/vms/<id>`) |
| Ready but no addresses | the guest agent in the guest and `agent: 1` on the template; on PVE 9 the token needs `VM.GuestAgent.Audit` |
| `TaskFailed` | `status.lastFailure` has Proxmox's exit status; the task log in the Proxmox UI has the rest |
| Delete hangs | the credentials Secret still exists; `status.nextAttempt` (a failed stop or destroy backs off) |

For a direct look at what the token sees, `go run ./hack/pvels <credentials.yaml>`
lists VMs with pool, marker, running tasks, and the log of the last failed
task.
