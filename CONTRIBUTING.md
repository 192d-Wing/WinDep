# Contributing to Our Projects, Version 1.0

**NOTE: This CONTRIBUTING.md is for software contributions. You do not need to follow the Developer's Certificate of Origin (DCO) process for commenting on the 192d Wing repository documentation, such as CONTRIBUTING.md, INTENT.md, etc. or for submitting issues.**

Thanks for thinking about using or contributing to this software ("Project") and its documentation!

- [Policy & Legal Info](#policy)
- [Getting Started](#getting-started)
- [Submitting an Issue](#submitting-an-issue)
- [Submitting Code](#submitting-code)

## Policy

### 1. Introduction

The project maintainer for this Project will only accept contributions using the Developer's Certificate of Origin 1.1 located at [developercertificate.org](https://developercertificate.org) ("DCO"). The DCO is a legally binding statement asserting that you are the creator of your contribution, or that you otherwise have the authority to distribute the contribution, and that you are intentionally making the contribution available under the license associated with the Project ("License").

### 2. Developer Certificate of Origin Process

Before submitting contributing code to this repository for the first time, you'll need to sign a Developer Certificate of Origin (DCO) (see below). To agree to the DCO, add your name and email address to the [CONTRIBUTORS.md](https://github.com/192d-Wing/WinDep/blob/main/CONTRIBUTORS.md) file. At a high level, adding your information to this file tells us that you have the right to submit the work you're contributing and indicates that you consent to our treating the contribution in a way consistent with the license associated with this software (as described in [LICENSE.md](https://github.com/192d-Wing/WinDep/blob/main/LICENSE.md)) and its documentation ("Project").

### 3. Important Points

Pseudonymous or anonymous contributions are permissible, but you must be reachable at the email address provided in the Signed-off-by line.

If your contribution is significant, you are also welcome to add your name and copyright date to the source file header.

U.S. Federal law prevents the government from accepting gratuitous services unless certain conditions are met. By submitting a pull request, you acknowledge that your services are offered without expectation of payment and that you expressly waive any future pay claims against the U.S. Federal government related to your contribution.

If you are a U.S. Federal government employee and use a `*.mil` or `*.gov` email address, we interpret your Signed-off-by to mean that the contribution was created in whole or in part by you and that your contribution is not subject to copyright protections.

### 4. DCO Text

The full text of the DCO is included below and is available online at [developercertificate.org](https://developercertificate.org):

```txt
Developer Certificate of Origin
Version 1.1

Copyright (C) 2004, 2006 The Linux Foundation and its contributors.
1 Letterman Drive
Suite D4700
San Francisco, CA, 94129

Everyone is permitted to copy and distribute verbatim copies of this
license document, but changing it is not allowed.

Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```

## Getting Started

Please see the [Getting Started](192d-Wing.github.io/WinDep/) page in the documents.

## Submitting an Issue

You can report a bug or request a feature by opening an issue in the
[issue tracker](https://github.com/192d-Wing/WinDep/issues).

Before opening an issue:

- Search existing issues to avoid duplicates.
- Confirm the behavior on the latest revision of `main`.

When opening an issue, please include:

- A clear, descriptive title and summary.
- Steps to reproduce, plus the expected and actual behavior.
- Relevant logs and artifacts where available (e.g. `X:\Windows\Temp\windep.log`,
  `X:\Windows\Temp\inventory.json`, or the DISM/`bcdboot` output).
- The affected hardware make/model, firmware version, Secure Boot state, and boot
  method (USB or network) when the issue is hardware- or boot-related.

**Do not include classified information, Controlled Unclassified Information (CUI),
personally identifiable information (PII), credentials, or any other sensitive data**
in issues or attachments. To report a security vulnerability, **do not** open a public
issue — contact the project maintainers privately.

## Submitting Code

1. **Agree to the DCO.** Add your name and email to
   [CONTRIBUTORS.md](https://github.com/192d-Wing/WinDep/blob/main/CONTRIBUTORS.md) as
   described in the [Policy](#policy) section above.
2. **Fork** the repository and create a topic branch off `main`, named for the change
   (e.g. `feat/policy-gpu-rule` or `fix/dism-progress-parse`).
3. **Make your change.** Keep it focused, and validate where practical — PowerShell must
   parse cleanly, and each script carries a validation checklist in its header. Note in the
   PR anything you could not test (e.g. steps requiring the ADK or real hardware).
4. **Follow [Conventional Commits](https://www.conventionalcommits.org/).** Prefix each
   commit with a type and optional scope, e.g. `feat(deploy): ...`, `fix(policy): ...`,
   `docs: ...`, `chore(build): ...`.
5. **Sign off every commit (DCO)** with `git commit -s`. The `Signed-off-by` line must
   match a name and email listed in `CONTRIBUTORS.md`.
6. **Keep secrets out of the repository.** Never commit `install.wim`, real per-machine
   configs, credentials, or the contents of `dev-server/` (all are ignored by
   `.gitignore` — keep it that way).
7. **Update `CHANGELOG.md`** under the `[Unreleased]` heading, and update documentation
   where relevant.
8. **Open a pull request** against `main`, complete the pull request template, and
   reference any related issue (e.g. `Closes #123`).

A maintainer will review your pull request. Address review feedback by pushing additional
commits (or by amending and force-pushing your branch, keeping every commit signed off).
