#!/usr/bin/env bash
# Build the multi-arch (amd64 + arm64) WinDep API image on Iron Bank UBI10.
#
# Uses Podman/Buildah by default (Containerfile + DoD tooling). Each arch pulls its
# own Iron Bank per-arch base tag; the binary is FIPS-enabled and cross-compiled, so
# no emulation is needed. Requires: `podman login registry1.dso.mil` and a login to
# the target registry.
#
#   REGISTRY=ghcr.io/192d-wing TAG=0.1.0 ./image/build.sh
set -euo pipefail

: "${ENGINE:=podman}"                                  # podman | docker
: "${REGISTRY:=ghcr.io/192d-wing}"
: "${IMAGE:=windep-api}"
: "${TAG:=0.1.0}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTEXT="${SCRIPT_DIR}/../api"
CONTAINERFILE="${SCRIPT_DIR}/Containerfile"

declare -A BASE=(
  [amd64]="registry1.dso.mil/ironbank/redhat/ubi/ubi10:10.2"
  [arm64]="registry1.dso.mil/ironbank/redhat/ubi/ubi10:10.2-arm64"
)

MANIFEST="${REGISTRY}/${IMAGE}:${TAG}"

if [ "${ENGINE}" = "podman" ]; then
  # Podman builds each arch straight into a manifest list.
  podman manifest rm "${MANIFEST}" 2>/dev/null || true
  podman manifest create "${MANIFEST}"
  for arch in amd64 arm64; do
    echo ">> building ${IMAGE}:${TAG} (${arch}) on ${BASE[$arch]}"
    podman build \
      --platform "linux/${arch}" \
      --build-arg BASE_IMAGE="${BASE[$arch]}" \
      --build-arg VERSION="${TAG}" \
      --manifest "${MANIFEST}" \
      -f "${CONTAINERFILE}" "${CONTEXT}"
  done
  podman manifest push --all "${MANIFEST}"
  echo ">> pushed ${MANIFEST} (amd64+arm64)"
else
  # Docker buildx: per-arch tags, then a manifest list.
  for arch in amd64 arm64; do
    echo ">> building ${IMAGE}:${TAG}-${arch} on ${BASE[$arch]}"
    docker buildx build \
      --platform "linux/${arch}" \
      --build-arg BASE_IMAGE="${BASE[$arch]}" \
      --build-arg VERSION="${TAG}" \
      -f "${CONTAINERFILE}" \
      -t "${MANIFEST}-${arch}" \
      --push "${CONTEXT}"
  done
  docker manifest create "${MANIFEST}" \
    --amend "${MANIFEST}-amd64" --amend "${MANIFEST}-arm64"
  docker manifest push "${MANIFEST}"
  echo ">> pushed ${MANIFEST} (amd64+arm64)"
fi
