# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). While the API is `v1alpha1`, any
minor release may change it.

Add an entry under `[Unreleased]` with every user-visible change. A release
renames that heading to the version and date (see
[docs/development.md](docs/development.md#releasing)).

## [Unreleased]

### Added

- `ProxmoxMachine` (`infrastructure.kube-vm.io/v1alpha1`): backs a KubeVM
  `VirtualMachine` with a Proxmox VE QEMU VM cloned from a template.
- Lifecycle driven from the portable `VirtualMachine` alone: clone, size
  (cores, memory, boot disk grow), native cloud-init (user, SSH keys,
  hostname, DNS, DHCP or static IPv4), power on/off with `Hard`, `Soft` and
  `TrySoft`, guest-agent addresses, and delete.
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
  resource listing, which lags. Paused VMs report `Paused`, not Ready.
- KubeVM core controller hosted in the same manager, plus an annotation
  nudge on the parent `VirtualMachine` so status changes after readiness
  are mirrored (works around the core not watching provider objects).
- Development tooling: in-memory Proxmox fake and `cmd/fakepve` HTTP
  server, envtest suite, `make mac-pve-iso` / `make mac-pve-run` for a real
  PVE (arm64 or emulated amd64) on an Apple Silicon Mac,
  `hack/pve-dev-setup.sh` for a throwaway PVE, and `hack/pvels` for
  inspecting one.
