# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html),
and commits follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/).

## [Unreleased]

## [0.1.0] - 2026-07-03

Initial release: a WinPE-based Windows 11 deployment platform with a graphical UI,
zero-touch provisioning, and a policy-gated data-collection phase.

### Added

- **WinPE build automation** (`Build/Build-WinPE.ps1`) — ADK-driven: adds optional
  components, injects the internal root CA and the deploy payload, sets `startnet.cmd`,
  builds a UEFI ISO, and stages the network-boot fileset (`bootmgfw.efi`/`BCD`/`boot.wim`).
- **Two delivery methods** — USB (MS-signed `bootmgr`) and network boot via MS-signed
  `bootmgfw.efi`: UEFI HTTPS Boot where firmware supports it, TFTP PXE fallback otherwise.
  Secure Boot stays enabled end to end (no iPXE).
- **WPF deployment UI** (`Deploy/DeployUI.xaml` + `DeployUI.ps1`) — interactive disk
  picker with typed-`ERASE` confirmation, live progress, and a zero-touch flow, themed in
  the DOW brand palette.
- **Deployment engine** (`Deploy/DeployEngine.psm1`) — GPT partitioning, HTTPS image
  download with progress, DISM apply, `bcdboot`, and tokenized `unattend.xml` injection.
- **Zero-Touch Provisioning** (`Deploy/Get-ZtpConfig.ps1`) — per-machine config pulled
  over HTTPS by BIOS serial, with auto-routing (per-machine config → zero-touch, otherwise
  interactive) and best-effort status reporting.
- **Full OOBE automation** (`Deploy/unattend.template.xml`) — bypasses OOBE prompts and the
  Microsoft-account requirement, creates a local admin, sets the computer name, and supports
  optional domain join.
- **Auto-reboot** — the UI reboots automatically 10 seconds after a successful deploy, with a
  "Reboot now" option to skip the wait.
- **Hardware/firmware inventory collection** (`Deploy/Get-Inventory.ps1`,
  `Schema/inventory.schema.json`) — make, model, serial, asset tag, chassis, BIOS, firmware
  type, Secure Boot state, TPM, CPU, RAM (+ DIMMs), disks (NVMe/SSD/HDD), NICs (make/model),
  and GPUs (make/model).
- **Policy hard gate** (`Deploy/Invoke-Policy.ps1`, `Server/policy/windep.rego`) — inventory
  is evaluated by Open Policy Agent before any disk is touched; `deny`/`hold` shows the failed
  checks and remediations. Fail-closed when the policy engine is unreachable.
- **Server reference** (`Server/`) — sample HTTPS layout, config samples, and hosting +
  DHCP vendor-class + OPA guidance.

[Unreleased]: https://example.com/WinDep/compare/v0.1.0...HEAD
[0.1.0]: https://example.com/WinDep/releases/tag/v0.1.0
