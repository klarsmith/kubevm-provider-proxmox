# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). While the API is `v1alpha1`, any
minor release may change it.

Add an entry under `[Unreleased]` with every user-visible change. A release
renames that heading to the version and date (see
[docs/development.md](docs/development.md#releasing)).

## [Unreleased]

## [0.1.0] - 2026-10-07

First release. Built on controller-runtime v0.25 and k8s.io v0.37 (KubeVM
itself pins v0.19 / v0.31; its core controller builds and passes envtest on
the newer versions); Go 1.26 or newer is required to build. Depends on
KubeVM at `vmware-tanzu/vm-operator@c2daa3ae` (`feature/kube-vm`).

### Added

- `ProxmoxMachine` (`infrastructure.kube-vm.io/v1alpha1`): backs a KubeVM
  `VirtualMachine` with a Proxmox VE QEMU VM cloned from a template.
- Lifecycle driven from the portable `VirtualMachine` alone: clone, size
  (cores, memory, boot disk grow), native cloud-init (SSH keys, hostname,
  DNS, DHCP or static IPv4; the cloud-init user is a provider-only field),
  power on/off with `Hard`, `Soft` and `TrySoft`, guest-agent addresses,
  and delete. Edits after first boot are re-applied: sizing while stopped,
  disk growth any time, cloud-init for the next boot.
- A setting Proxmox rejects (e.g. an unparsable SSH key) is reported as
  `InvalidConfiguration` and not retried as an API error.
- Validation and guards: a static address without `staticIPPrefixLength`
  is rejected before any Proxmox call; `deleteOnTermination: false` blocks
  deletion instead of destroying disks; LXC containers count as VMID
  holders; a reserved VMID held by a VM the token cannot see is given up
  after three refused clones.
- Pool guard: the manager only creates, changes or deletes VMs in the pool
  named by `--allowed-pool` (default `kubevm-dev`).
- Crash-safe creation: the VMID is reserved in status before the clone is
  sent, and every VM carries a `kubevm-uid=` marker, so retries never clone
  twice and delete sweeps strays.
- Proxmox tasks are tracked by UPID and never block a reconcile. A VM
  lifecycle task that is already running is adopted rather than
  duplicated; consoles and backups are ignored.
- Failed tasks back off exponentially (1 min doubling to 30 min), recorded
  in `status.consecutiveFailures`, `status.lastFailure` and
  `status.nextAttempt`. A new power request resets the backoff, and only
  failed stops or destroys delay deletion.
- Decisions use each VM's live state (`/status/current`), not the cluster
  resource listing, which lags. Paused VMs report `Paused`, not Ready, and
  are stopped before destroy. A VM mid-migration is re-listed, not reported
  gone.
- Every created VM carries the tag `kubevm` and a `kubevm-uid=` description
  line. Running VM lifecycle tasks started outside Kubernetes are waited
  for (their failures are not counted); consoles and backups are ignored.
- Manager: `--allowed-pool`, `--leader-elect`, health probes, optional
  metrics. Credentials Secrets are read directly (RBAC: `get` only). One
  Proxmox HTTP client per credentials.
- KubeVM core controller hosted in the same manager, plus an annotation
  nudge on the parent `VirtualMachine` so status changes after readiness
  are mirrored (works around the core not watching provider objects).
- Development tooling: in-memory Proxmox fake and `cmd/fakepve` HTTP
  server, envtest suite, `make mac-pve-iso` / `make mac-pve-run` for a real
  PVE (arm64, emulated amd64, or x86 nested on another Proxmox),
  `hack/pve-dev-setup.sh` for a throwaway PVE, `hack/pvels` (with `--get`
  for raw API reads) for inspecting one, and `hack/fakepve` to run the fake
  Proxmox inside kind for an in-cluster test of the manager.

[Unreleased]: https://github.com/klarsmith/kubevm-provider-proxmox/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/klarsmith/kubevm-provider-proxmox/releases/tag/v0.1.0
