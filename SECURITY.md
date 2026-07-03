# Security Policy

## Supported versions

| Version | Supported |
|---------|-----------|
| 0.1.x   | ✅ |
| < 0.1   | ❌ |

Security fixes are made against `main` and the latest tagged release.

## Reporting a vulnerability

**Please do not open a public issue for security vulnerabilities.**

Report privately using **GitHub's private vulnerability reporting**:
*Security → Advisories → Report a vulnerability* on
<https://github.com/192d-Wing/WinDep/security/advisories/new>. If that is unavailable, contact
the maintainers directly (see [CONTRIBUTORS.md](CONTRIBUTORS.md)).

When reporting, please include:

- affected component (WinPE agent, deployment engine, OPA policy, telemetry API, or CI/build),
- version / commit,
- a description and, where possible, reproduction steps or a proof of concept,
- the impact you observed.

**Do not include classified information, Controlled Unclassified Information (CUI), personally
identifiable information (PII), credentials, or live host/network details** in a report. Provide
the minimum needed to reproduce.

## Coordinated disclosure

- We aim to acknowledge a report within **3 business days**.
- We will work with you on a fix and a coordinated disclosure timeline (typically ≤ 90 days).
- Please give us reasonable time to remediate before any public disclosure.

## Scope and hardening notes

This project deploys operating systems and reports deployment telemetry; keep the following in mind:

- **Deployment runs on a controlled/isolated provisioning VLAN.** The WinPE boot leg may be
  cleartext (TFTP fallback); sensitive payloads (config, image, unattend, status/logs) ride
  in-WinPE HTTPS to an internal CA. See [`README.md`](README.md).
- **No secrets in `boot.wim`.** It contains only WinPE, the deploy scripts, the *public* root CA
  cert, and the server URL. Real per-machine configs (with credentials) and `install.wim` are
  git-ignored and must never be committed.
- **Telemetry API** is stateless and, by default, unauthenticated on `/api/*`; protect it with a
  NetworkPolicy and/or the optional `API_TOKEN`. `/api/machines` and `/metrics` are information-
  disclosure surfaces — keep them cluster-internal where possible.
- **Supply chain:** container images build on hardened Iron Bank UBI10 with FIPS 140-3 Go crypto;
  Go dependencies are scanned with `govulncheck`; GitHub Actions are pinned to commit SHAs; and
  CodeQL runs on every change.

## Things that are intentionally not vulnerabilities

- The cleartext WinPE boot leg on the provisioning VLAN (documented trade-off; Secure Boot still
  validates the OS loader chain).
- A missing `API_TOKEN` in a deployment where the API is already network-isolated.
