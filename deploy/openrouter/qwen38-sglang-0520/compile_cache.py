#!/usr/bin/env python3
"""Publish and restore immutable, hardware-scoped inference JIT caches."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import pathlib
import re
import shutil
import stat
import tempfile
from datetime import UTC, datetime
from typing import Any

SCHEMA = "infercrane.dev/compiled-cache-release/v1"
MANIFEST = "manifest.json"
SENTINEL = ".infercrane-compiled-cache"


def _sha256(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(8 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _safe_root(raw: str) -> pathlib.Path:
    path = pathlib.Path(raw)
    if not path.is_absolute() or path == pathlib.Path("/") or path == pathlib.Path.home():
        raise ValueError("cache paths must be absolute and cannot be / or the home directory")
    return path


def _files(root: pathlib.Path) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for path in sorted(root.rglob("*")):
        if path.name in {MANIFEST, SENTINEL}:
            continue
        mode = path.lstat().st_mode
        if stat.S_ISLNK(mode):
            raise ValueError(f"compiled cache cannot contain symlinks: {path}")
        if path.is_dir():
            continue
        if not path.is_file():
            raise ValueError(f"compiled cache contains a non-regular file: {path}")
        relative = path.relative_to(root).as_posix()
        rows.append({"path": relative, "bytes": path.stat().st_size, "sha256": _sha256(path)})
    return rows


def _compatibility(args: argparse.Namespace) -> dict[str, str]:
    return {
        "runtime_image": args.runtime_image,
        "model_revision": args.model_revision,
        "gpu_arch": args.gpu_arch,
        "cuda_version": args.cuda_version,
    }


def _manifest_digest(manifest: dict[str, Any]) -> str:
    body = json.dumps(manifest, sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(body).hexdigest()


def publish(args: argparse.Namespace) -> None:
    source = _safe_root(args.source)
    releases = _safe_root(args.releases)
    if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}", args.release):
        raise ValueError("compiled cache release name is invalid")
    destination = releases / args.release
    if not source.is_dir() or not any(source.iterdir()):
        raise ValueError("source cache must be a non-empty directory")
    if destination.exists():
        raise ValueError(f"compiled cache release already exists: {destination}")
    files = _files(source)
    if not files:
        raise ValueError("source cache contains no regular files")
    releases.mkdir(parents=True, exist_ok=True)
    staging = pathlib.Path(tempfile.mkdtemp(prefix=f".{args.release}.", dir=releases))
    try:
        shutil.copytree(source, staging / "cache", dirs_exist_ok=True, symlinks=False)
        manifest = {
            "schema_version": SCHEMA,
            "release": args.release,
            "created_at": datetime.now(UTC).isoformat(),
            "compatibility": _compatibility(args),
            "files": files,
        }
        manifest["content_digest"] = _manifest_digest(
            {"compatibility": manifest["compatibility"], "files": files}
        )
        manifest_path = staging / MANIFEST
        manifest_path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
        manifest_path.chmod(0o444)
        os.rename(staging, destination)
    except Exception:
        shutil.rmtree(staging, ignore_errors=True)
        raise
    print(json.dumps({"release": str(destination), "manifest_sha256": _sha256(destination / MANIFEST), "files": len(files), "bytes": sum(row["bytes"] for row in files)}, sort_keys=True))


def _load_verified_release(args: argparse.Namespace) -> tuple[pathlib.Path, dict[str, Any]]:
    release = _safe_root(args.release_dir)
    manifest_path = release / MANIFEST
    if not manifest_path.is_file() or manifest_path.is_symlink():
        raise ValueError("compiled cache manifest must be a regular file")
    actual_manifest_sha = _sha256(manifest_path)
    if not args.manifest_sha256 or actual_manifest_sha != args.manifest_sha256:
        raise ValueError("compiled cache manifest digest mismatch")
    manifest = json.loads(manifest_path.read_text())
    if manifest.get("schema_version") != SCHEMA:
        raise ValueError("unsupported compiled cache manifest schema")
    if manifest.get("compatibility") != _compatibility(args):
        raise ValueError("compiled cache compatibility identity mismatch")
    cache = release / "cache"
    observed = _files(cache)
    if observed != manifest.get("files"):
        raise ValueError("compiled cache content does not match its manifest")
    expected_content = _manifest_digest({"compatibility": manifest["compatibility"], "files": observed})
    if manifest.get("content_digest") != expected_content:
        raise ValueError("compiled cache content digest mismatch")
    return cache, manifest


def restore(args: argparse.Namespace) -> None:
    source, manifest = _load_verified_release(args)
    destination = _safe_root(args.destination)
    destination.parent.mkdir(parents=True, exist_ok=True)
    if destination.exists():
        if any(destination.iterdir()):
            if not (destination / SENTINEL).is_file():
                raise ValueError("refusing to replace a non-InferCrane cache directory")
            shutil.rmtree(destination)
        else:
            destination.rmdir()
    staging = pathlib.Path(tempfile.mkdtemp(prefix=f".{destination.name}.", dir=destination.parent))
    try:
        shutil.copytree(source, staging, dirs_exist_ok=True, symlinks=False)
        (staging / SENTINEL).write_text(str(manifest["content_digest"]) + "\n")
        os.rename(staging, destination)
    except Exception:
        shutil.rmtree(staging, ignore_errors=True)
        raise
    print(json.dumps({"restored": str(destination), "content_digest": manifest["content_digest"], "files": len(manifest["files"])}, sort_keys=True))


def parser() -> argparse.ArgumentParser:
    value = argparse.ArgumentParser()
    commands = value.add_subparsers(dest="command", required=True)
    for name in ("publish", "restore"):
        command = commands.add_parser(name)
        command.add_argument("--runtime-image", required=True)
        command.add_argument("--model-revision", required=True)
        command.add_argument("--gpu-arch", required=True)
        command.add_argument("--cuda-version", required=True)
    publish_parser = commands.choices["publish"]
    publish_parser.add_argument("--source", required=True)
    publish_parser.add_argument("--releases", required=True)
    publish_parser.add_argument("--release", required=True)
    restore_parser = commands.choices["restore"]
    restore_parser.add_argument("--release-dir", required=True)
    restore_parser.add_argument("--manifest-sha256", required=True)
    restore_parser.add_argument("--destination", required=True)
    return value


def main() -> None:
    args = parser().parse_args()
    if args.command == "publish":
        publish(args)
    else:
        restore(args)


if __name__ == "__main__":
    main()
