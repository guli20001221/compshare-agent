#!/usr/bin/env python3
"""Reject registry manifests that the production Docker 1.12 daemon cannot pull."""

import json
import sys


DOCKER_MANIFEST = "application/vnd.docker.distribution.manifest.v2+json"
DOCKER_CONFIG = "application/vnd.docker.container.image.v1+json"
DOCKER_GZIP_LAYER = "application/vnd.docker.image.rootfs.diff.tar.gzip"


def fail(message: str) -> None:
    raise SystemExit(f"incompatible registry manifest: {message}")


try:
    manifest = json.load(sys.stdin)
except (json.JSONDecodeError, OSError) as exc:
    fail(f"cannot parse JSON: {exc}")

if manifest.get("schemaVersion") != 2:
    fail(f"schemaVersion={manifest.get('schemaVersion')!r}, want 2")

media_type = manifest.get("mediaType")
if media_type != DOCKER_MANIFEST:
    fail(f"mediaType={media_type!r}, want a single Docker schema-v2 manifest")

config_type = (manifest.get("config") or {}).get("mediaType")
if config_type != DOCKER_CONFIG:
    fail(f"config mediaType={config_type!r}, want {DOCKER_CONFIG!r}")

layers = manifest.get("layers")
if not isinstance(layers, list) or not layers:
    fail("manifest has no layers")

bad_layers = [
    (index, layer.get("mediaType"))
    for index, layer in enumerate(layers)
    if not isinstance(layer, dict) or layer.get("mediaType") != DOCKER_GZIP_LAYER
]
if bad_layers:
    fail(f"non-gzip/non-Docker layer media types: {bad_layers!r}")

print(f"manifest ok: Docker schema v2, gzip layers={len(layers)}")
