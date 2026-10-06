# Development

## Layout

```
api/v1alpha1/                 ProxmoxMachine type
controllers/proxmoxmachine/   reconciler: controller, resolve, create, provision,
                              power, observe, addresses, delete, nudge
controllers/controllers.go    hosts this controller and the KubeVM core on one manager
internal/proxmox/             Client interface and the HTTP client
internal/proxmox/proxmoxfake/ in-memory Fake and its REST Server (tests, cmd/fakepve;
                              never linked into the manager)
internal/link/                two-sided VirtualMachine <-> ProxmoxMachine check
cmd/manager/                  the manager binary
cmd/fakepve/                  fake Proxmox API server for local runs
hack/pve-dev-setup.sh         prepares a throwaway PVE (pool, template, token)
hack/mac-pve/                 real PVE in QEMU on an Apple Silicon Mac
hack/pvels/                   read-only inspector for a PVE
config/                       CRDs (ours + vendored KubeVM), RBAC, manager, samples
docs/findings.md              design decisions and what real Proxmox taught us
```

## Make targets

```sh
make test       # unit tests + envtest; never needs a Proxmox
make lint       # golangci-lint
make generate   # deepcopy, CRD, RBAC (controller-gen)
make verify     # fails if generated code is stale
make install    # apply both CRDs to the current kubectl context
make run        # run the manager against the current kubectl context
```

The module builds with Go 1.23 (go.mod). The pinned tools (controller-gen,
golangci-lint) need a current Go.

## Tests

- **Unit tests** (`controllers/proxmoxmachine`) drive the reconciler with
  controller-runtime's fake client and `proxmoxfake.Fake`. The fake models what
  the design depends on: tasks that finish over time, pool membership that
  lands only when a clone finishes, a resource listing that can lag
  (`ListedStatus`), and injectable failures (`FailTask`, `AlwaysFailTask`,
  `Err`). Lost status writes are simulated with an interceptor
  (`failStatusPatch`).
- **HTTP client tests** (`internal/proxmox`) run `HTTPClient` over real HTTP
  against `proxmoxfake.Server`.
- **envtest** (`controllers`) runs the real manager, with the KubeVM core
  and this controller, against a real API server and the fake Proxmox. It
  covers create through delete via the `VirtualMachine` only. Skipped unless
  `KUBEBUILDER_ASSETS` is set; `make test` sets it.

A regression found on real Proxmox gets a test that fails without the fix.
`docs/findings.md` names them.

## Running locally

### Against the fake Proxmox

```sh
kind create cluster --name kubevm-dev
make install
kubectl create namespace team-a
kubectl apply -f config/samples/credentials-fakepve.yaml
go run ./cmd/fakepve &        # http://127.0.0.1:8006, one template "debian-12"
go run ./cmd/manager
kubectl apply -f config/samples/virtualmachine.yaml
kubectl get vm,pxm -n team-a -o wide
```

### Against a real PVE on an Apple Silicon Mac

```sh
brew install qemu
# download proxmox-ve_9.2-1-arm64.iso into .local/pve-vm/ (or proxmox-ve_9.2-1.iso
# into .local/pve-vm-amd64/ for x86)
make mac-pve-iso                 # unattended ISO; add PVE_ARCH=amd64 for x86
make mac-pve-run                 # first run installs and exits; run again to boot
```

On first boot, `hack/pve-dev-setup.sh` creates the pool, a Debian template
and a token, then prints a credentials Secret to the serial console
(`.local/pve-vm/serial.log`). Save it as `.local/credentials.yaml` and apply
it, then run the manager and apply `config/samples/virtualmachine-pve-dev.yaml`
with your SSH key filled in.

| | arm64 | amd64 |
|---|---|---|
| Acceleration | HVF, near native | none (TCG), very slow |
| API / SSH | `https://127.0.0.1:8006` / port 2222 | `:8007` / port 2223 |
| Guests inside | emulated (`kvm: 0`, no nested virtualization on M1/M2) | emulated |
| Practical for | full lifecycle tests | API paths; guest boots take hours |

Stop a test PVE cleanly with `ssh -p 2222 root@127.0.0.1 poweroff` (2223
for amd64). Its disk is kept; `make mac-pve-run` boots it again.

Inspect what the token sees:

```sh
go run ./hack/pvels .local/credentials.yaml
```

### Against a throwaway PVE elsewhere

Run `hack/pve-dev-setup.sh` as root on a PVE with no VMs (a nested PVE VM
is ideal). The script refuses to run on a node that already has VMs: it
rewrites apt sources and `/etc/network/interfaces`.

## Releasing

1. In `CHANGELOG.md`, rename `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD`
   and add a fresh empty `## [Unreleased]` above it.
2. Commit, then tag: `git tag vX.Y.Z && git push origin vX.Y.Z`.
3. The release workflow builds `ghcr.io/<owner>/kubevm-provider-proxmox:vX.Y.Z`
   (amd64 and arm64), renders `install.yaml`, and publishes a GitHub release
   with that version's changelog section as notes. It fails if the
   changelog has no section for the tag.

## Upgrading the KubeVM dependency

KubeVM still lives on the `feature/kube-vm` branch of `vmware-tanzu/vm-operator`.
To move to a newer commit:

```sh
go get github.com/vmware-tanzu/vm-operator/external/kubevm@<commit> \
       github.com/vmware-tanzu/vm-operator/external/kubevm/controller@<commit>
# set KUBEVM_COMMIT in the Makefile to the same commit, then:
make kubevm-crd test
```
