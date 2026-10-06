# Security

Please report vulnerabilities privately through GitHub's
[security advisories](../../security/advisories/new) for this repository,
not in a public issue.

Worth knowing when deploying:

- The manager holds Proxmox API tokens read from Secrets. Give each token
  the least-privilege role in [docs/proxmox-setup.md](docs/proxmox-setup.md),
  scoped to one pool, never `Administrator`.
- `--allowed-pool` is the boundary the controller enforces. It never acts
  on a VM outside that pool.
- A `ProxmoxMachine` is only acted on when its own annotation names the
  `VirtualMachine` that references it, so a `VirtualMachine` alone cannot
  claim someone else's machine.
- `insecureSkipVerify: "true"` disables TLS verification. Use `caBundle`
  instead outside test setups.
