#!/bin/sh
# Build on a current BuildKit host, then publish an image Docker 1.12.6 can pull.

set -eu

if [ "$#" -ne 1 ]; then
    echo "usage: $0 <private-registry/image:immutable-tag>" >&2
    exit 2
fi

IMAGE=$1
IMAGE_BASENAME=${IMAGE##*/}
case "$IMAGE_BASENAME" in
    *:latest)
        echo "image must use an immutable tag, not :latest: $IMAGE" >&2
        exit 2
        ;;
    *:*) ;;
    *)
        echo "image must use an explicit immutable tag: $IMAGE" >&2
        exit 2
        ;;
esac
case "$IMAGE" in
    *,*)
        echo "image name must not contain a comma: $IMAGE" >&2
        exit 2
        ;;
esac

VCS_REF=$(git rev-parse HEAD)

docker buildx build \
    --platform linux/amd64 \
    --provenance=false \
    --sbom=false \
    --build-arg "VCS_REF=$VCS_REF" \
    --output "type=registry,name=$IMAGE,oci-mediatypes=false,compression=gzip,force-compression=true" \
    .

# Do not trust the exporter/registry defaults: Docker 1.12 cannot consume an
# OCI index or modern zstd layers.  Verify what the registry actually serves.
docker buildx imagetools inspect --raw "$IMAGE" \
    | python3 deploy/docker/verify-docker-v2-manifest.py

echo "published Docker 1.12-compatible image: $IMAGE"
