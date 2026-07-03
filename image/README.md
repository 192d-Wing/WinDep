# WinDep API image (FIPS 140-3, Iron Bank UBI10, multi-arch)

Builds the [telemetry API](../api/) container. Build context is `../api`; the image
definition is a **`Containerfile`** (Podman/Buildah — the Iron Bank/DoD default).

- **Runtime base**: Iron Bank `registry1.dso.mil/ironbank/redhat/ubi/ubi10:10.2`
  (amd64) and `...:10.2-arm64` (arm64), hardened and ATO-friendly.
- **FIPS**: Go's native **FIPS 140-3** Cryptographic Module (`GOFIPS140`). Pure Go, so
  `CGO_ENABLED=0`, static binary, cross-compiled for every arch — no OpenSSL/CGO/QEMU.
- **Arch matrix**: amd64 + arm64.
- **Hardening**: non-root (`USER 1001`), no shell needed, read-only-rootfs compatible
  (the app only writes to stdout).

## FIPS 140-3

The binary is built with `GOFIPS140=v1.0.0` — the CMVP-validated **Go Cryptographic Module**
snapshot bundled with the Go 1.24+ toolchain. This bakes the runtime `fips140` GODEBUG to `on`,
and the Containerfile also sets `GODEBUG=fips140=on` explicitly. Use `fips140=only` to *reject*
non-approved algorithms outright.

> **ATO note:** confirm the current CMVP certificate/status for the Go Cryptographic Module version
> you ship (`certified.txt` / `inprocess.txt` under `$(go env GOROOT)/lib/fips140/`). If your AO
> requires a specific CMVP-validated **OpenSSL** module instead, build with the Red Hat
> `go-toolset` (dynamically links UBI's FIPS OpenSSL) — that path needs `CGO_ENABLED=1`, a per-arch
> builder, and a FIPS-mode host. The native path above avoids all of that.

Verify a build is FIPS-capable:

```bash
GOFIPS140=v1.0.0 GODEBUG=fips140=on ./windep-api   # starts iff power-on self-tests pass
```

## Build (Podman — recommended)

```bash
podman login registry1.dso.mil          # Iron Bank base images
podman login ghcr.io      # target registry

REGISTRY=ghcr.io/192d-wing TAG=0.1.0 ./image/build.sh
```

`build.sh` builds each arch from its Iron Bank per-arch base straight into a manifest list and
pushes `…/windep-api:0.1.0` (multi-arch). Set `ENGINE=docker` to use Docker buildx instead.

Single arch, by hand:

```bash
podman build -f image/Containerfile \
  --platform linux/amd64 \
  --build-arg BASE_IMAGE=registry1.dso.mil/ironbank/redhat/ubi/ubi10:10.2 \
  --build-arg VERSION=0.1.0 \
  -t ghcr.io/192d-wing/windep-api:0.1.0-amd64 api
```

## Build args

| Arg | Default | Purpose |
|-----|---------|---------|
| `BASE_IMAGE` | `…/ubi10:10.2` | Iron Bank runtime base (matrix sets the per-arch tag) |
| `BUILDER_IMAGE` | `golang:1.25-alpine` | Go toolchain (≥1.25; override with Iron Bank go-toolset for a full IB supply chain) |
| `GOFIPS140` | `v1.0.0` | Go Cryptographic Module version |
| `VERSION` | `0.0.0-dev` | Stamped into OCI image labels |

## CI

[`.github/workflows/image.yml`](../.github/workflows/image.yml) builds the amd64/arm64 **matrix**
with Buildah and assembles the manifest list. It needs `IRONBANK_USER`/`IRONBANK_TOKEN` and target
registry credentials as repository secrets, and self-hosted or emulated runners for arm64.
