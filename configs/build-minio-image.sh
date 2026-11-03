#!/usr/bin/env bash
# Build the MinIO server image from the official release binary.
#
#   configs/build-minio-image.sh [release]
#
# Why: the upstream images (minio/minio on Docker Hub, quay.io/minio/minio) are
# no longer publicly pullable, while docker-compose.yaml expects the image to
# exist under the upstream tag. This script downloads the release binary from
# an official mirror and builds the image with configs/minio.Dockerfile.
set -euo pipefail

RELEASE="${1:-${MINIO_VERSION:-RELEASE.2025-04-22T22-12-26Z}}"
IMAGE="${MINIO_IMAGE:-minio/minio:${RELEASE}}"

case "$(uname -m)" in
    x86_64 | amd64) ARCH=amd64 ;;
    aarch64 | arm64) ARCH=arm64 ;;
    *)
        echo "unsupported architecture: $(uname -m)" >&2
        exit 1
        ;;
esac

MIRRORS=("${MINIO_MIRROR:-https://dl.minio.org.cn}" "https://dl.min.io")
URL_PATH="server/minio/release/linux-${ARCH}/archive/minio.${RELEASE}"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fetched=0
for base in "${MIRRORS[@]}"; do
    url="${base}/${URL_PATH}"
    echo "==> fetching ${url}"
    if curl -fsSL --retry 2 -o "$work/minio" "$url"; then
        fetched=1
        break
    fi
    echo "    failed, trying the next mirror"
done

if [[ $fetched -ne 1 ]]; then
    echo "could not download MinIO ${RELEASE} from any mirror" >&2
    exit 1
fi

chmod +x "$work/minio"
echo "==> binary: $(du -h "$work/minio" | cut -f1) (linux/${ARCH})"

cp "$(dirname "$0")/minio.Dockerfile" "$work/Dockerfile"
docker build -t "$IMAGE" "$work"
echo "==> built ${IMAGE}"
