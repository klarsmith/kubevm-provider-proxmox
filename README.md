# kubevm-provider-proxmox

A [KubeVM](https://github.com/vmware-tanzu/vm-operator/tree/feature/kube-vm/external/kubevm)
infrastructure provider for Proxmox VE. It backs a portable
`VirtualMachine` (`kube-vm.io/v1alpha1`) with a Proxmox QEMU VM: cloned from
a template, sized, configured through cloud-init, powered on and off, and
destroyed, all from the `VirtualMachine`. The KubeVM core controller runs in
the same process.

```
$ kubectl get vm -n team-a -o wide
NAME       POWER       READY   REASON    PROVIDER-ID             PRIMARY-IP
smoke-01   PoweredOn   True    Running   proxmox://pve-dev/100   10.99.0.181
```

That output is from a real run against Proxmox VE 9.2.9.

## Status

`v1alpha1`, not released yet. KubeVM itself is pre-release and lives on the
`feature/kube-vm` branch of `vmware-tanzu/vm-operator`.

Tested end to end against real Proxmox VE 9.2.9 (arm64):
- create, with the IP from the guest agent
- SSH key and hostname injection
- static IPv4
- power on/off, including `Soft` refusal and `TrySoft` fallback
- delete

The same on PVE 9.2.2 amd64 under real (nested) KVM: create, IP, SSH,
power, delete. Details, and the bugs real Proxmox found, are in
[docs/findings.md](docs/findings.md).

Not implemented yet, and reported as `UpToDate=False/UnsupportedByProvider`
when asked for: data disks, cloud-init user/network data from Secrets,
named instance types, network references, IPv6 static addresses,
`Suspended`, and `deleteOnTermination: false`.

## How it works, briefly

- You write a `VirtualMachine` and a `ProxmoxMachine` that name each other
  (two-sided link; one-sided references are ignored).
- The controller copies the portable fields into the `ProxmoxMachine`
  spec, reserves a VMID, clones the template into the allowed pool, applies
  sizing and cloud-init, grows the boot disk, and starts the VM.
- Every Proxmox operation is a task. Its UPID is recorded in status and
  polled, never waited on. Failed tasks back off exponentially.
- The controller only acts inside one Proxmox pool (`--allowed-pool`).

## Quick start

Without a Proxmox, using the bundled fake API:

```sh
kind create cluster --name kubevm-dev
make install
kubectl create namespace team-a
kubectl apply -f config/samples/credentials-fakepve.yaml
go run ./cmd/fakepve &
go run ./cmd/manager
kubectl apply -f config/samples/virtualmachine.yaml
kubectl get vm,pxm -n team-a -o wide
```

Against a real Proxmox: prepare a pool, a template and a token
([docs/proxmox-setup.md](docs/proxmox-setup.md)), create the credentials
Secret, then apply a linked pair like
[config/samples/virtualmachine.yaml](config/samples/virtualmachine.yaml).

In-cluster: `make image-build IMG=<image>` and `make deploy`, or point Flux
at `config/`.

## Docs

- [User guide](docs/user-guide.md): field reference, conditions, power,
  networking, delete, troubleshooting
- [Proxmox setup](docs/proxmox-setup.md): pool, template, token privileges
- [Development](docs/development.md): tests, a real PVE on a Mac, releasing
- [Findings](docs/findings.md): design decisions and what real Proxmox
  taught us
- [Changelog](CHANGELOG.md), [Contributing](CONTRIBUTING.md),
  [Security](SECURITY.md)

## License

[Apache-2.0](LICENSE)
