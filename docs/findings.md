# Findings

Where satisfying the KubeVM contract on Proxmox VE took a real design
decision, what was verified (and against what), and where this provider
differs from `kubevm-provider-container`. Same purpose as that provider's
`docs/findings.md`: the next person to touch this reads it first.

Verified against `vmware-tanzu/vm-operator@feature/kube-vm`, commit
`c2daa3ae` (2026-09-22), unless stated otherwise.

## Upstream state (Phase 0, 2026-10-04)

- KubeVM has **not** been split out yet. It still lives in
  `vm-operator/external/kubevm` on `feature/kube-vm`. This provider is a
  standalone module that depends on `external/kubevm` and
  `external/kubevm/controller` by pseudo-version. No `replace` directives
  were needed.
- The contract's status paths match `controller/internal/contract/contract.go`
  exactly: `addresses[]{interface,type,address}`, `powerState`, `providerID`,
  `providerMetadata` (string map), and `conditions[]` (`InfrastructureReady`,
  `UpToDate`).
- `external/kubevm-provider-aws` is cited throughout the provider guide but
  is **not in the repository**, and never was in the branch history. The
  container provider is the only reference implementation available.
- No Proxmox provider effort exists, upstream or in the CNCF sandbox issue
  (#517). That issue plans KubeVirt as the second provider.
- Enums: `PowerState` = `PoweredOn | PoweredOff | Suspended`;
  `PowerOpMode` = `Hard | Soft | TrySoft`.

## The core does not see status changes after readiness

**This one is worth raising upstream.**

The core controller (`controllers/virtualmachine`) watches only
`VirtualMachine`. It re-reads the provider object when the VirtualMachine
changes, and it polls (`pollRequeueDelay`, 10 s) **only while the provider is
neither ready nor reporting addresses** (`!ready && len(addresses) == 0` in
`reconcileStatus`).

The consequence: any provider-side change that happens after the machine is
ready never reaches `VirtualMachine.status` until the next full resync, which
is 10 h with controller-runtime's default. The cases:

- a power-off or power-on finishing,
- an address appearing or changing,
- the VM disappearing out of band.

The trigger is a power flip. The core reconciles on the spec edit, reads the
provider while it still says Ready with addresses, and doesn't requeue. The
provider then finishes the operation, and nothing tells the core.

`kubevm-provider-container` has the same gap. It doesn't show up there
because its README checks the `ContainerMachine`, not the `VirtualMachine`,
after a power change. The envtest here caught it by asserting on the
VirtualMachine.

**Workaround in this provider:** after writing status, the controller stamps
`infrastructure.kube-vm.io/provider-status-hash` on the parent
VirtualMachine. The value is a hash of exactly the contract paths. The core's
watch has no predicates, so the annotation change makes it re-read
(`controllers/proxmoxmachine/nudge.go`). This needs `patch` on
`virtualmachines`, which the manager already has because it hosts the core.

**Proper fix (upstream, written and tested, not yet proposed):** the core
watches adopted provider objects. On first adoption of a GVK it adds a
metadata-only watch on that kind and maps events back through the controller
owner reference it already sets. The patch is about 60 lines, plus a new
envtest spec in the core's own suite; the spec fails before the change and
passes after. With the patched core and the nudge switched off, this
provider's envtest passes the full lifecycle. Once that lands upstream and
the KubeVM pin is bumped, `nudge.go` can be deleted.

## Lost status writes must not repeat Proxmox operations

Found by the envtest once the core reacted faster to provider events. The
controller made a Proxmox call (shutdown). The status write that would have
recorded the task's UPID then failed with an optimistic-lock conflict (a
stale cache read). The retry, seeing no pending task and a VM still
running, sent a **second** shutdown. On real PVE that call fails on the VM
lock, or queues another task behind the first.

Clones were already covered by the UID marker. Power and delete operations
weren't. The fix uses Proxmox's own record: before starting any operation,
the controller lists the VM's running tasks
(`/nodes/{node}/tasks?vmid=X&source=active`). If one is running, it adopts
it as `pendingTask` instead of starting another. This also covers a manager
restart mid-operation, and waits out tasks nobody here started (a migration
from the web UI). Covered by `TestLostStatusWriteDoesNotRepeatPowerOp`
(start, shutdown, delete), which uses a fake-client interceptor to drop the
status write.

## The resource listing lags; act on live status

**Found on the x86 run (PVE 9.2.2).** `/cluster/resources` is served from a
cluster-wide cache that pvestatd refreshes every few seconds, so its
`status` can disagree with the VM's real state. Deciding on it caused two
visible problems:

- **A false Ready.** A VM crash-looping on start showed
  `PoweredOn / Ready / Running` on the VirtualMachine. In the same
  reconcile, the listing said "running" while the guest-agent call
  answered "VM 100 is not running".
- **Repeated stops on delete.** The VM's task history showed four
  `qmstop: OK` before `qmdestroy`. Each stop landed, the listing still said
  "running", and the controller stopped it again.

The fix: the listing is used only to find VMs (ID, node, pool). Every
decision about a VM uses `/nodes/{node}/qemu/{vmid}/status/current`, which
is one extra call per reconcile, for the one VM being acted on.
`TestStaleListingNeverReportsACrashedVMReady` and
`TestDeleteWithStaleListingStopsOnce` drive the fake with a lagging listing.
Both fail with the live read disabled and pass with it.

This is the Proxmox counterpart of the AWS provider's eventual-consistency
handling, and worth a line in the provider guide: "observed state" can mean
a cache, even on a platform that answers synchronously.

## Failed tasks back off

The earlier design requeued a failed task after one minute. That did not
stop event-triggered reconciles (a status write, a VirtualMachine edit),
which retried at once. A VM that could not start was hit with start calls
continuously, and `UpToDate` read `True/Applied` the whole time.

Now:

- A failed task increments `status.consecutiveFailures`, records
  `status.lastFailure`, and sets `status.nextAttempt`. The wait starts at
  1 min, doubles per failure, and is capped at 30 min.
- No new Proxmox task (clone, resize, power, delete) starts before
  `nextAttempt`. Observation continues meanwhile.
- While retries are on hold, `UpToDate` is `False/TaskFailed` with the
  failure, the count, and the next attempt time. `InfrastructureReady` is
  `False/TaskFailed` whenever the VM is not in the asked-for power state.
- The next task that succeeds clears the streak.

The gate is in status, not in a requeue delay, precisely because requeue
delays do not hold back event-triggered reconciles.
`TestFailedStartsBackOffAndRecover` uses a controllable clock: one retry,
none within the backoff, one after it, a longer second backoff, and a reset
on recovery.

## Pre-commit review

An independent review of the controller before the first commit found
nine ways it could misbehave against a real Proxmox. All are fixed, each
with a regression test in `controllers/proxmoxmachine/review_test.go`. For
the two that could clone a second VM (1 and 2 below), the test was shown
to fail with the fix disabled.

1. **The conflict retry could overwrite a newer status.** A reconcile
   working from a stale cache (`vmid` still 0) could reserve a second VMID
   and patch it over the real one. The retry now proceeds only if the
   stored status is the one the reconcile started from.
2. **A clone's placeholder config was mistaken for someone else's VM.** PVE
   first writes a config holding only `lock: clone`; the description
   carrying the marker lands later. The clone lock is now checked before
   the marker. *Not verified on real PVE; taken from how `clone_vm` is
   understood to work, and harmless if it is wrong.*
3. **Delete could send a clone**, through the shared resume path, or block
   forever if the template was gone. Delete now settles a reservation
   itself: it waits while a `qmclone` task runs on the template (the clone
   task is filed under the source VMID), and otherwise releases it.
4. **A failed clone became terminal `VMGone`.** PVE removes the partial VM.
   The machine now returns to "reserved, not sent" and re-sends after the
   backoff.
5. **Any running task was adopted**, so an open console (`vncproxy`) or a
   backup (`vzdump`) made the machine NotReady and blocked power changes.
   Only VM lifecycle tasks and migrations are adopted now.
6. **"Not found" matched too broadly.** Any error mentioning "does not
   exist" (a storage, a bridge) counted as a missing VM. Only a missing VM
   config or task counts now, and a task that vanished from the log clears
   `pendingTask` instead of failing every reconcile.
7. **A paused VM read as Ready.** `/status/current` says `running` for a
   VM paused on an I/O error; only `qmpstatus` says `paused`. It is now
   reported as `Paused`.
8. **An unreadable credentials Secret parked the machine** until the next
   resync (about 10 h), because Secrets are not watched. It is now returned
   as an error and retried with backoff.
9. **An LXC container on the reserved ID** answered every clone with
   "already exists" forever, because the listing dropped containers. They
   are now kept as ID holders, and the reservation moves on.

Also: `OutsidePool` and `VMGone` are re-checked on the resync period, and
changing the power request clears an earlier failure backoff.

### Rechecked on real PVE 9.2.9 (arm64) after the review

- **The clone task is filed under the template's VMID** (`"id":"9000"`,
  UPID `...:qmclone:9000:...`), as fix 3 assumes. It is visible in
  `/nodes/{node}/tasks?source=active&vmid=<template>` while it runs.
- **New bug found: that list reports a running task's status as
  `"RUNNING"`, in upper case.** `ActiveTasks` kept only `"running"`, so on
  real Proxmox it dropped every running task. Both "adopt a running task
  instead of duplicating it" and fix 3's "is a clone in flight" were blind
  there; they passed only against the fake, which sent no status. Fixed
  (case-insensitive), and `proxmoxfake.Server` now answers with `"RUNNING"` as PVE
  does.
- **`/status/current` has `qmpstatus`** (`"running"` for a running VM),
  which fix 7 reads to detect a paused VM.
- **Two machines at once** (VMIDs 100 and 101): both Ready with IPs, both
  deleted cleanly, 0 errors in the manager log.

Not exercised on real PVE: a lost status write (it cannot be forced from
outside), and an actually paused VM (pausing needs root).

## Async tasks

Clone, start, shutdown, stop, resize (PVE ≥ 8) and delete all return a task
UPID. The controller stores it in `status.pendingTask` (with
`status.pendingOp`), returns, and polls `/nodes/{node}/tasks/{upid}/status`
on later reconciles. A reconcile never blocks on a task.

- The node used for polling is parsed from the UPID itself
  (`UPID:<node>:...`), so it does not depend on stored state.
- `exitstatus` of `OK`, or of `WARNINGS: n`, is success. Anything else is
  surfaced verbatim on `InfrastructureReady=False/TaskFailed`.
- Resize on older PVE returns `null`, meaning it ran synchronously. The
  client returns `""` and the controller treats that as already done.

This is the contract finding a hypervisor-with-tasks provider adds next to
AWS (eventual consistency) and the container provider (synchronous CLI):
"transitional" can last minutes and is owned by a task, not by the object.

## Idempotency: VM names are not unique, nextid races

- **Identity marker.** At clone time, `kubevm-uid=<ProxmoxMachine UID>` goes
  on the first line of the VM `description`. The clone call writes it
  atomically with the new VM's config. Tags would be nicer, but the clone API
  does not take tags (they need a second call, which leaves a crash window),
  and the PVE tag charset does not allow `=`. A cosmetic `kubevm` tag is
  added during provisioning.
- **Adopt after crash.** Before any clone, the controller lists
  `/cluster/resources?type=vm`, reads the config of every non-template VM
  **in the allowed pool**, and looks for the marker. If found, it adopts that
  VMID. Covered by `TestAdoptsVMWhoseIDWasNeverRecorded`.
- **Reserve the VMID first.** The controller takes an ID from
  `/cluster/nextid` and writes it to `status.vmid` (with
  `pendingOp: Clone`) **before** sending the clone. The clone then targets
  exactly that ID. Proxmox refuses a second clone into an existing ID, so no
  retry can create a second VM. If the reserved ID turns out to hold a VM
  without our marker (another client won the race), the reservation is
  dropped and a new ID taken (`TestVMIDRaceRetriesWithFreshID`). A reserved
  ID that exists but stays invisible is waited on, never dropped on a timer:
  a slow clone looks exactly the same.

### What the real PVE run found here

The first real run (PVE 9.2.9) **cloned the template twice** for one
machine. The marker-lookup design had a blind spot no fake exposed:

1. The clone was sent. The status write recording its UPID lost an
   optimistic-lock race with the KubeVM core, which was adding its owner
   reference at the same moment.
2. The retry ran the marker lookup. On real PVE, **a VM mid-clone belongs to
   no pool until the clone task finishes**, and so is **invisible to a
   pool-scoped token**: `/cluster/resources` does not list it at all.
3. Seeing nothing, the retry cloned again into a fresh ID.

The fixes, all verified on the same PVE afterwards:

- Reserve the VMID before cloning (above).
- `statusWriter` retries a status write on conflict against the current
  resourceVersion. Status is written by this controller alone, so this is
  safe.
- Delete sweeps every VM in the pool carrying the marker, and settles an
  unrecorded in-flight clone before releasing the finalizer.

The fake now models the late pool membership, and regression tests drop the
clone's status write (`TestLostStatusWriteDoesNotRepeatPowerOp/clone`,
`TestDeleteWhileUnrecordedCloneIsInFlight`,
`TestDeleteSweepsDuplicateMarkerVMs`). On the second real run, the UPID
write was lost again, before the conflict retry existed. The controller
re-sent the clone into the reserved ID, got "already exists", waited for the
clone to become visible, and adopted it. That produced one VM.

## Real-PVE smoke test (2026-10-05)

Environment: Proxmox VE **9.2.9 arm64**, running in QEMU/HVF on an M1 Pro
(`make mac-pve-iso mac-pve-run`, unattended install, `hack/pve-dev-setup.sh`
as the first-boot hook). Guests run emulated (`kvm: 0`; no nested
virtualization on M1). The manager ran on the Mac against kind.

Passed through the portable `VirtualMachine` alone:

| Step | Result |
|---|---|
| create | one clone into the reserved VMID, then sizing, cloud-init, NIC and 8 GiB grow, then start |
| observe | `Ready`, `proxmox://pve-dev/100`, IP `10.99.0.181` from the guest agent on interface `primary` (MAC matching works) |
| power off (TrySoft) | `PoweredOff`, address cleared; mirrored onto the VirtualMachine |
| power on | `PoweredOn`, IP back |
| delete | VM destroyed; also swept the duplicate from the first run |
| errors | 0 in the manager log with the final build |

Confirmed on real PVE:

- **The token role works as written in `hack/pve-dev-setup.sh`.** That
  includes `VM.GuestAgent.Audit`, which exists on PVE 9 and is what the
  agent read needs.
- **The sshkeys encoding works end to end.** Percent-encoding with `%20`
  for spaces is accepted by the API and decoded correctly by cloud-init: an
  SSH login as `debian` with the injected key succeeded. On that guest,
  `hostname` was `smoke-01` (from `network.hostName`), `cloud-init status`
  was `done`, and `qemu-guest-agent` was `active`.
- **`/cluster/status` on an unclustered node.** It has no cluster entry, so
  the providerID uses the node name.
- **A VM mid-clone has no pool and is invisible to a pool-scoped token.**
- **The static-IP path works.** The portable `addresses: ["10.99.0.50"]`
  plus the provider-only `staticIPPrefixLength: 24` and `gateway` became
  `ipconfig0: ip=10.99.0.50/24,gw=10.99.0.1`. In the guest that showed up
  as `eth0 10.99.0.50/24`, `default via 10.99.0.1 proto static`, and
  `nameserver 1.1.1.1` (from `network.nameservers`), and DNS resolved. The
  address is outside the bridge's DHCP range, so it cannot have come from a
  lease.
- **Soft and TrySoft behave as the KubeVM enum defines them**, tested with a
  guest made to refuse shutdown (`systemctl mask poweroff.target` and logind
  `HandlePowerKey=ignore`; this blocks both the ACPI path and the
  guest-agent path that Proxmox's `shutdown` uses).
  - **Soft:** after the 180 s timeout the task failed with Proxmox's
    `VM quit/powerdown failed`. That surfaced as
    `UpToDate=False/TaskFailed`, the VM stayed **running**, and nothing
    escalated to a stop. Re-attempts now follow the failure backoff (see
    "Failed tasks back off"); at the time of this run they came every
    minute or so.
  - **TrySoft:** switching the same request to TrySoft (`forceStop=1`)
    powered the VM off after the timeout. The VirtualMachine showed
    `PoweredOff/Stopped`, and `UpToDate` returned to `Applied` by itself.

### x86 (amd64) run, PVE 9.2.2, fully emulated

The same Mac, with `make mac-pve-iso mac-pve-run PVE_ARCH=amd64`: QEMU TCG,
SeaBIOS, and a BIOS template with an `ide2` cloud-init drive.

- Clone into the reserved ID, provisioning, and resize worked on the BIOS /
  `ide2` template path.
- Under TCG, `/dev/kvm` exists inside the emulated host (QEMU emulates
  SVM), but cannot supply the CPU features PVE's default guest CPU asks
  for. Every start failed with `kvm: Host doesn't support requested
  features`. This is an environment limit; `hack/pve-dev-setup.sh` now
  also uses `kvm: 0` when `systemd-detect-virt` reports `qemu`.
- That failure exposed the two problems above (lagging listing, no
  backoff). Delete still cleaned up correctly mid crash-loop.
- With `kvm: 0`, the guest started, but booting x86-in-x86 under two layers
  of software emulation is too slow to finish: after 35 min it had used
  190 MB and had no DHCP lease. IP, guest agent and SSH were therefore
  verified on arm64 only; the provider code is the same for both.
- On the build with live status and backoff: power-off (TrySoft) sent
  exactly one `qmshutdown` and was mirrored as `PoweredOff`. Power-on and
  delete worked, leaving only the template. The manager log showed two
  benign errors: a status-write conflict in the delete path, which did not
  yet use `statusWriter`, and a `not found` when a stale-cache reconcile
  released an already-deleted object. Both are fixed: delete uses
  `statusWriter`, and finalizer release ignores NotFound.

### x86 (amd64) on real KVM: nested PVE 9.2.2 (2026-10-06)

A PVE VM (`make mac-pve-iso PVE_ARCH=nested`, unattended, static IP) on a
production Proxmox host with nested virtualization (AMD), on an internal
Hetzner vSwitch network. Guests ran under real KVM (`systemd-detect-virt`
→ `kvm`).

Passed through the portable `VirtualMachine` alone: create, guest-agent IP
(`10.99.0.155`), SSH login with the injected key (hostname `smoke-01`, user
`debian`, `x86_64`, cloud-init `done`), power off (TrySoft), power on, and
delete, leaving only the template. This closes the amd64 gap: the BIOS /
`ide2` template path works end to end.

Environment lessons, now handled by the scripts:

- **A vSwitch uplink has MTU 1400.** The installer configured 1500, which
  the virtio NIC rejects, so `ifreload` failed. `hack/pve-dev-setup.sh` now
  gives the NAT bridge and DHCP the uplink's MTU, and points guests at the
  PVE host's own dnsmasq for DNS instead of a public resolver.
- **The network's gateway is a router VM (pfSense), not the PVE host.** The
  host does not forward (`ip_forward 0`). The static answer file takes the
  gateway as input (`STATIC_GW`).
- **The setup's stdout carried `qm` progress lines** as well as the
  Secret, so redirecting it to a file produced invalid YAML. stdout now
  carries only the Secret.

## The node can change

HA and live migration move VMs between nodes. The stored node is never used
for API calls. Each reconcile resolves the current node from
`/cluster/resources`. For that reason `providerID` is
`proxmox://<cluster>/<vmid>`, with no node. On an unclustered host,
`<cluster>` is the node name, since `/cluster/status` has no cluster entry
there.

## Pool guard

The manager takes `--allowed-pool` (default `kubevm-dev`). The controller:

- refuses to act at all if `spec.pool` differs (`OutsidePool`, zero Proxmox
  calls);
- clones only into the allowed pool;
- only adopts marker matches inside the allowed pool
  (`TestMarkerOutsidePoolIsNotAdopted`);
- re-checks the VM's pool every reconcile. If someone moves it out, the
  controller stops acting on it (`OutsidePool`). On delete it releases the
  finalizer **without** deleting the VM (`TestVMMovedOutOfPoolIsLeftAlone`).

Templates may live outside the pool. Reading and cloning a template does not
change it.

## Power mapping

| KubeVM | Proxmox |
|---|---|
| `PoweredOn` | `status/start` |
| off, `Hard` | `status/stop` |
| off, `Soft` | `status/shutdown` (timeout 180 s, `forceStop=0`). A guest that does not comply fails the task. Reported on `UpToDate=False/TaskFailed`, retried after 1 min, **never** escalated to stop |
| off, `TrySoft` (and unset) | `status/shutdown` (timeout 180 s, `forceStop=1`). Proxmox itself stops the VM after the timeout |
| `Suspended` | `UpToDate=False/UnsupportedByProvider` (Phase 6) |

Observed: `running` → `PoweredOn`, `stopped` → `PoweredOff`. While a power
task is in flight, `status.powerState` is absent.

Proxmox implements all three PowerOpModes cleanly, unlike EC2 (no Soft) and
GCE (TrySoft only). `common_types.go` lists vSphere as the only platform with
all three. Proxmox is a second.

## Provisioning before first boot

After the clone task finishes, and before the first start, the controller:

1. reads the config;
2. applies only differing values via `PUT /config` (synchronous): `cores`,
   `memory` (MiB, rounded up), `ciuser`, `sshkeys`, `nameserver`,
   `searchdomain`, `netN`, `ipconfigN`, and the tag;
3. grows the boot disk if `bootDisk.sizeGiB` is larger. It never shrinks.

Once that is done it sets `status.provisioned`. After first boot, `cores` and
`memory` are applied only while the VM is stopped. If it is running,
`UpToDate` says "applied the next time the VM is powered off". Disk growth
works at any time.

- **Boot disk key.** It is not assumed to be `scsi0`. The controller reads
  `boot: order=...` first, then legacy `bootdisk`, then
  `scsi*/virtio*/sata*/ide*`, skipping cdrom and cloud-init drives
  (`TestBootDisk`).
- **sshkeys encoding.** The stored value must be percent-encoded with spaces
  as `%20`, not `+`, and it is then form-encoded again on the wire. *VERIFY on
  real PVE.* This is the most commonly reported gotcha.
- **NICs.** For each portable interface N, the controller sets `netN` and
  keeps the template's model and MAC (`withBridge`), changing only the
  bridge. A new NIC gets `virtio` and a fresh MAC.

## Addresses

- Addresses come from the QEMU guest agent
  (`agent/network-get-interfaces`). Loopback and link-local addresses are
  dropped. All reported addresses are `InternalIP`.
- Guest NICs are matched to `netN` by MAC, so an address carries the
  **portable** interface name (`primary`) where one was declared, and the
  guest's own name (`eth0`) otherwise.
- If the agent is not answering, the VM still reports
  `InfrastructureReady=True` while running. Addresses stay empty, `UpToDate`
  carries a note, and the controller polls every 10 s.

## Networking gotcha: bare IPs

KubeVM `interfaces[].addresses` are bare IPs. Proxmox `ipconfigN` needs a
CIDR, and a gateway for anything routed. The MVP takes both from
provider-only fields (`staticIPPrefixLength`, `gateway`). A static address
without a prefix length is rejected as `InvalidConfiguration` before any
Proxmox call. Static IPv6 and `dhcp6` are `UnsupportedByProvider`.

This is the second contract finding worth raising upstream. Platforms that
configure the guest directly (Proxmox cloud-init, vSphere customization)
need the prefix and gateway. Platforms that hand out addresses (EC2, GCE)
don't. Phase 6 moves this to a `ProxmoxNetwork` object referenced by
`interfaces[].network`.

## Delete

The finalizer holds the object. The flow:

1. Wait for any pending task, including a clone in flight
   (`TestDeleteMidClone`).
2. Hard-stop if running.
3. `DELETE ?purge=1&destroy-unreferenced-disks=1`.
4. Poll, then release the finalizer.

A VM that is already gone, or that has left the pool, releases the finalizer
straight away. `bootDisk.deleteOnTermination: false` blocks deletion with
`UnsupportedByProvider` rather than destroying disks that were asked to be
kept.

## Go client: own thin REST client, not go-proxmox

`luthermonson/go-proxmox` (checked at `d78ab04`, 2026-09-30) covers every
endpoint needed. It wasn't used because:

- it required Go 1.25 at the time, while the KubeVM modules pinned 1.23
  (since moot: this module now needs 1.26 for k8s.io 0.37);
- it addresses VMs through objects fetched per node, which costs extra calls
  and assumes a stable node;
- a much larger surface would sit behind the fake.

The provider needs 16 calls. `internal/proxmox.HTTPClient` is about 300
lines with token auth, sits behind the `Client` interface, and is tested over
real HTTP against `proxmoxfake.Server`.

## Unsupported in the MVP (reported, not ignored)

All of these surface on `UpToDate=False/UnsupportedByProvider` with a
message:

- `instanceType.name`
- data `disks`
- bootDisk snapshot/blank sources
- `storageClassName`, `volumeAttributesClassName`
- `interfaces[].network`, `publicIP`, `dhcp6`, static IPv6
- `bootstrap.cloudInit.userData/networkData` (CHECKPOINT 5, deferred)
- `failureDomain`, `scheduling.spot`, `tags`
- `Suspended`

## Token privileges

These privileges are for PVE 8/9. **VERIFY** them on the target version:
privilege names change between releases. Grant them on the pool (or on
`/vms/<id>` for the template) and on the storage and bridge in use:

- `VM.Clone` on the template; `VM.Allocate` on `/pool/kubevm-dev` (new VMID)
- `VM.Config.Disk`, `VM.Config.CPU`, `VM.Config.Memory`,
  `VM.Config.Network`, `VM.Config.Options`, `VM.Config.Cloudinit`
- `VM.PowerMgmt`, `VM.Audit`
- Guest agent: **`VM.GuestAgent.Audit` on PVE 9**. PVE 9 removed
  `VM.Monitor`; on PVE 8 use `VM.Monitor`.
- `Datastore.AllocateSpace` and `Datastore.Audit` on the clone storage
- `SDN.Use` on the bridge/zone (PVE 8+)
- `Sys.Audit` on `/` for `/cluster/status`
