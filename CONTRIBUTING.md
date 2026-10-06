# Contributing

Issues and pull requests are welcome.

- Read [docs/development.md](docs/development.md) for the layout, the
  tests, and how to get a real Proxmox to test against.
- `make verify lint test` must pass. None of it needs a Proxmox.
- A behaviour change against real Proxmox needs a test that fails without
  it. If the Proxmox behaviour itself was a surprise, add it to
  [docs/findings.md](docs/findings.md).
- Add a line under `## [Unreleased]` in [CHANGELOG.md](CHANGELOG.md) for
  anything a user would notice.
- New `ProxmoxMachine` fields: say in the field comment whether it is
  resolved from the `VirtualMachine` or provider-only, and run
  `make generate`.

By contributing you agree your contribution is licensed under the
[Apache License 2.0](LICENSE).
