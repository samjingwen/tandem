#!/usr/bin/env python3
"""Install only the checksum-pinned wt binary into a project-local directory."""

import hashlib
import io
import os
import platform
import subprocess
import sys
import tarfile
import tempfile
import urllib.request
from pathlib import Path

VERSION = "0.81.0"
# Official max-sixty/worktrunk v0.81.0 release asset digests.
ARCHIVES = {
    ("Darwin", "arm64"): (
        "aarch64-apple-darwin",
        "b9c3f2f8071554cf88500466d65d35e18ad5edad5762479a1f14bcddd5ab7882",
    ),
    ("Darwin", "x86_64"): (
        "x86_64-apple-darwin",
        "211fa3585afa0ce243de0f007fbe2d8b86c5c88c85eda17d09311b32e8533714",
    ),
    ("Linux", "x86_64"): (
        "x86_64-unknown-linux-musl",
        "0c253f216985ce9cd7b202a11606b5ce6aa934b1019fd1fbb9b7ce2b47b1c74a",
    ),
    ("Linux", "aarch64"): (
        "aarch64-unknown-linux-musl",
        "7a334d2e64c577afd0247e2411eff673360ab1f18897564f2d2e5d5374fbd74b",
    ),
}
ARCHIVE_LIMIT = 32 << 20
BINARY_LIMIT = 64 << 20


def binary_from_archive(data, digest, target):
    if len(data) > ARCHIVE_LIMIT or hashlib.sha256(data).hexdigest() != digest:
        raise ValueError("Worktrunk archive checksum/size verification failed")
    # Never extract paths, symlinks, git-wt, policy, or release documentation.
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:xz") as archive:
        members = [m for m in archive if m.name == f"worktrunk-{target}/wt"]
        if len(members) != 1:
            raise ValueError("Worktrunk archive must contain exactly one wt binary")
        member = members[0]
        if not member.isfile() or not 0 < member.size <= BINARY_LIMIT:
            raise ValueError("Worktrunk wt member must be a bounded regular file")
        with archive.extractfile(member) as stream:
            binary = stream.read(BINARY_LIMIT + 1)
        if len(binary) != member.size:
            raise ValueError("Worktrunk wt member is incomplete")
        return binary


def atomic_write(path, data, mode):
    if path.is_symlink() or (path.exists() and not path.is_file()):
        raise ValueError("refusing a nonregular Worktrunk installation path")
    with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as stream:
        temporary = Path(stream.name)
        try:
            stream.write(data)
            stream.flush()
            os.fchmod(stream.fileno(), mode)
            os.fsync(stream.fileno())
            os.replace(temporary, path)
        finally:
            temporary.unlink(missing_ok=True)


def prepare(destination):
    key = (platform.system(), platform.machine())
    if key not in ARCHIVES:
        raise ValueError(f"unsupported Worktrunk fixture platform: {key}")
    target, digest = ARCHIVES[key]
    destination = Path(os.path.abspath(destination))
    for path in (destination, *destination.parents):
        if path.is_symlink():
            raise ValueError("Worktrunk installation parents must be canonical")
    destination.mkdir(mode=0o700, parents=True, exist_ok=True)
    binary_dir = destination / "bin"
    if binary_dir.is_symlink():
        raise ValueError("Worktrunk binary directory must not be a symlink")
    binary_dir.mkdir(mode=0o700, exist_ok=True)
    archive_path = destination / f"worktrunk-{target}.tar.xz"
    if archive_path.is_symlink():
        raise ValueError("Worktrunk archive cache must not be a symlink")
    if archive_path.exists():
        with archive_path.open("rb") as stream:
            data = stream.read(ARCHIVE_LIMIT + 1)
    else:
        url = f"https://github.com/max-sixty/worktrunk/releases/download/v{VERSION}/{archive_path.name}"
        with urllib.request.urlopen(url, timeout=30) as response:
            data = response.read(ARCHIVE_LIMIT + 1)
    binary = binary_from_archive(data, digest, target)
    atomic_write(archive_path, data, 0o600)
    executable = binary_dir / "wt"
    atomic_write(executable, binary, 0o700)
    # Re-extract and re-verify on every preparation, not just a version string
    # from a possibly modified cached executable. No global installation/hooks.
    result = subprocess.run(
        [str(executable), "--version"],
        check=True,
        capture_output=True,
        timeout=5,
        env={"PATH": "/usr/bin:/bin"},
    )
    if result.stdout.strip() != f"wt v{VERSION}".encode():
        raise ValueError("verified Worktrunk binary reported an unexpected version")
    print(f"Verified Worktrunk {VERSION}: {executable}")


if __name__ == "__main__":
    try:
        if len(sys.argv) != 2:
            raise ValueError("usage: prepare-worktrunk.py PROJECT_LOCAL_DESTINATION")
        prepare(sys.argv[1])
    except (OSError, ValueError, tarfile.TarError, subprocess.SubprocessError) as error:
        sys.exit(f"Worktrunk preparation failed: {error}")
