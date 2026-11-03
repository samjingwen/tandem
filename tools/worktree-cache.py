#!/usr/bin/env python3
"""Reuse compatible, quiescent local caches through Worktrunk's copy consumer."""

import argparse
import fcntl
import fnmatch
import hashlib
import json
import os
import platform
import runpy
import stat
import subprocess
import tempfile
from contextlib import ExitStack, contextmanager
from pathlib import Path

METADATA = Path(".tmp/worktree-cache")
ROOTS = {
    "node": ["plugin/node_modules"],
    "rust": ["target"],
    "worktrunk": [".tmp/worktrunk"],
    "audit": [".tmp/rust-tools/bin/cargo-audit"],
}
RUST_PAYLOAD = ["target/.rustc_info.json", "target/CACHEDIR.TAG"] + [
    f"target/{profile}/{directory}"
    for profile in ("debug", "release")
    for directory in (".fingerprint", "deps", "build")
]
PRIVATE_NAMES = {
    ".env",
    ".git",
    ".opencode",
    ".venv",
    "__pycache__",
    ".aws",
    ".ssh",
    ".npm",
    ".tandem",
    ".npmrc",
    ".netrc",
    "auth.json",
    "credentials",
    "credentials.json",
    "credentials.toml",
    "credentials.ini",
    ".cargo-lock",
}
PRIVATE_PATTERNS = (
    ".env.*",
    "*.db",
    "*.db-*",
    "*.sqlite*",
    "*.pid",
    "*.sock",
    ".package-cache*",
)
FLAGS = {
    "node": (
        "npm_config_arch",
        "npm_config_platform",
        "npm_config_libc",
        "CC",
        "CXX",
        "CFLAGS",
        "CXXFLAGS",
        "LDFLAGS",
        "MACOSX_DEPLOYMENT_TARGET",
        "SDKROOT",
    ),
    "rust": (
        "RUSTFLAGS",
        "CARGO_ENCODED_RUSTFLAGS",
        "CARGO_BUILD_TARGET",
        "RUSTC",
        "RUSTC_WRAPPER",
        "RUSTC_WORKSPACE_WRAPPER",
        "CC",
        "CXX",
        "CFLAGS",
        "CXXFLAGS",
        "LDFLAGS",
        "MACOSX_DEPLOYMENT_TARGET",
        "SDKROOT",
    ),
}


class CopyError(Exception):
    """A started copy failed; block dependent hooks instead of trusting partial data."""


def output(command, root):
    return subprocess.check_output(
        command,
        cwd=root,
        text=True,
        timeout=30,
        env={**os.environ, "RUSTUP_AUTO_INSTALL": "0"},
    ).strip()


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def canonical(root, relative):
    """Refuse redirects before opening even our own cache metadata."""
    path = root / relative
    for item in (path, *path.parents):
        if item == root:
            break
        if item.is_symlink():
            raise ValueError(f"cache path is a symlink: {relative}")
    return path


def signature(root, group):
    files = [".worktreeinclude", "tools/worktree-cache.py"]
    if group == "node":
        files += ["plugin/package.json", "plugin/package-lock.json"]
        runtime = [
            output(
                [
                    "node",
                    "-p",
                    "JSON.stringify({versions:process.versions,arch:process.arch,platform:process.platform})",
                ],
                root,
            ),
            output(["npm", "--version"], root),
        ]
    else:
        if group == "rust" and (
            os.environ.get("CARGO_TARGET_DIR")
            or os.environ.get("CARGO_BUILD_BUILD_DIR")
        ):
            raise ValueError("custom Cargo build directories are not copied")
        files += ["rust-toolchain.toml"]
        if group == "rust":
            files += ["Cargo.toml", "Cargo.lock"]
            files += [
                str(p.relative_to(root))
                for p in sorted(root.glob("crates/*/Cargo.toml"))
            ]
        else:
            files += ["Makefile"]
        runtime = [output(["rustc", "-vV"], root), output(["cargo", "--version"], root)]
        if group == "audit":
            runtime.append(
                output(
                    ["make", "--no-print-directory", "-s", "cache-audit-version"], root
                )
            )
    flags = json.dumps(
        {key: os.environ.get(key, "") for key in FLAGS.get(group, FLAGS["rust"])},
        sort_keys=True,
    )
    return {
        "schema": 1,
        "platform": [
            platform.system(),
            platform.machine(),
            platform.release(),
            *platform.libc_ver(),
        ],
        "runtime": runtime,
        "inputs": {name: digest(canonical(root, name)) for name in files},
        # Build flags may contain local paths. Do not persist their values.
        "flags": hashlib.sha256(flags.encode()).hexdigest(),
    }


@contextmanager
def cache_lock(root, relative, exclusive=False, create=False):
    path = canonical(root, relative)
    if create:
        path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a+b" if create else "rb") as stream:
        mode = fcntl.LOCK_EX if exclusive else fcntl.LOCK_SH
        fcntl.flock(stream.fileno(), mode | fcntl.LOCK_NB)
        yield


def record(root, group):
    value = {"signature": signature(root, group)}
    if group == "node":
        value["installedLock"] = digest(
            canonical(root, "plugin/node_modules/.package-lock.json")
        )
    if group == "audit":
        value["installedBinary"] = digest(canonical(root, ROOTS["audit"][0]))
    destination = canonical(root, METADATA / f"{group}.json")
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(
        mode="w", dir=destination.parent, delete=False
    ) as stream:
        temporary = Path(stream.name)
        try:
            json.dump(value, stream, sort_keys=True)
            stream.flush()
            os.fsync(stream.fileno())
            os.replace(temporary, destination)
        finally:
            temporary.unlink(missing_ok=True)


def compatible(source, destination, group):
    metadata = canonical(source, METADATA / f"{group}.json")
    if metadata.stat().st_size > 65536:
        raise ValueError("cache metadata exceeds its size bound")
    value = json.loads(metadata.read_text())
    if not isinstance(value, dict):
        raise ValueError("invalid cache metadata")
    expected = signature(destination, group)
    if value["signature"] != expected or signature(source, group) != expected:
        raise ValueError("lockfiles, runtime, flags or platform differ")
    if group == "node" and value["installedLock"] != digest(
        canonical(source, "plugin/node_modules/.package-lock.json")
    ):
        raise ValueError("installed npm lock changed since preparation")
    if group == "audit" and value["installedBinary"] != digest(
        canonical(source, ROOTS["audit"][0])
    ):
        raise ValueError("cargo-audit binary changed since preparation")


def safe_tree(root):
    """Worktrunk copies selected directories whole: inspect names before copying.

    Do not open private entries. Fail closed for the entire cache if one exists.
    Public dependency lockfiles are data, not in-use process coordination locks.
    """

    # Optional compiler metadata and the audit executable are individual leaves.
    if not root.exists():
        return
    mode = root.lstat().st_mode
    if stat.S_ISREG(mode):
        return
    if not stat.S_ISDIR(mode):
        raise ValueError("cache contains a special file")

    def fail(error):
        raise error

    for directory, dirs, files in os.walk(root, followlinks=False, onerror=fail):
        for name in dirs + files:
            path = Path(directory) / name
            private = name in PRIVATE_NAMES or any(
                fnmatch.fnmatchcase(name, pattern) for pattern in PRIVATE_PATTERNS
            )
            process_lock = name.endswith(".lock") and name not in {
                "yarn.lock",
                "Cargo.lock",
                "bun.lock",
            }
            if private or process_lock:
                raise ValueError(
                    "cache contains a private/runtime/control entry; leaving it entirely uncopied"
                )
            mode = path.lstat().st_mode
            if not (stat.S_ISREG(mode) or stat.S_ISDIR(mode) or stat.S_ISLNK(mode)):
                raise ValueError("cache contains a special file")
            if path.is_symlink() and (
                Path(os.readlink(path)).is_absolute()
                or not path.resolve().is_relative_to(root)
            ):
                raise ValueError(
                    "cache contains an absolute link or a link outside its own directory"
                )


def prepare_node(root, cold):
    with cache_lock(root, METADATA / "node.lock", exclusive=True, create=True):
        if not cold:
            try:
                compatible(root, root, "node")
                print("Reusing compatible plugin/node_modules; use --cold for npm ci.")
                return
            except (OSError, ValueError, KeyError):
                pass
        canonical(root, METADATA / "node.json").unlink(missing_ok=True)
        subprocess.run(
            [
                "npm",
                "--prefix",
                "plugin",
                "ci",
                "--ignore-scripts",
                # This checkout needs development tools regardless of ambient
                # NODE_ENV/omit/production settings or disabled executable links.
                "--include=dev",
                "--include=optional",
                "--include=peer",
                "--bin-links=true",
                "--install-strategy=hoisted",
                "--install-links=true",
            ],
            cwd=root,
            check=True,
        )
        record(root, "node")


def prepare_audit(root, destination):
    if destination.resolve() != root / ".tmp/rust-tools":
        version = output(
            ["make", "--no-print-directory", "-s", "cache-audit-version"], root
        )
        subprocess.run(
            [
                "cargo",
                "install",
                "--locked",
                "--root",
                str(destination),
                "--version",
                version,
                "cargo-audit",
            ],
            cwd=root,
            check=True,
        )
        return
    with cache_lock(root, METADATA / "audit.lock", exclusive=True, create=True):
        try:
            compatible(root, root, "audit")
            print("Reusing verified compatible cargo-audit.")
            return
        except (OSError, ValueError, KeyError):
            pass
        canonical(root, METADATA / "audit.json").unlink(missing_ok=True)
        version = output(
            ["make", "--no-print-directory", "-s", "cache-audit-version"], root
        )
        subprocess.run(
            [
                "cargo",
                "install",
                "--locked",
                "--force",
                "--root",
                str(root / ".tmp/rust-tools"),
                "--version",
                version,
                "cargo-audit",
            ],
            cwd=root,
            check=True,
        )
        if (
            output([str(root / ROOTS["audit"][0]), "--version"], root)
            != f"cargo-audit {version}"
        ):
            raise ValueError(
                "installed cargo-audit version does not match the project pin"
            )
        record(root, "audit")


def archive(root):
    preparation = runpy.run_path(str(root / "tools/prepare-worktrunk.py"))
    target, checksum = preparation["ARCHIVES"][(platform.system(), platform.machine())]
    relative = f".tmp/worktrunk/worktrunk-{target}.tar.xz"
    if digest(canonical(root, relative)) != checksum:
        raise ValueError("Worktrunk archive does not match the pinned digest")
    return relative


def copy(source, destination, dry_run):
    source = source.resolve()
    if source == destination:
        print("Cache source is the destination; nothing to copy.")
        return
    common = ["git", "rev-parse", "--path-format=absolute", "--git-common-dir"]
    if output(common, source) != output(common, destination):
        raise ValueError("cache source must be a worktree of this repository")
    if digest(canonical(source, ".worktreeinclude")) != digest(
        destination / ".worktreeinclude"
    ):
        raise ValueError("source and destination cache allowlists differ")
    branch = output(["git", "symbolic-ref", "--short", "HEAD"], source)
    exclusions = []
    selected = []
    with ExitStack() as locks:
        for group, roots in ROOTS.items():
            try:
                if group in {"node", "audit"}:
                    # Own the destination before inspecting or mutating it, so a
                    # concurrent project prepare cannot install into our copy.
                    locks.enter_context(
                        cache_lock(
                            destination,
                            METADATA / f"{group}.lock",
                            exclusive=True,
                            create=True,
                        )
                    )
                if any((destination / name).exists() for name in roots):
                    raise ValueError(
                        "destination already has this cache; not merging or overwriting"
                    )
                if group == "worktrunk":
                    if digest(source / "tools/prepare-worktrunk.py") != digest(
                        destination / "tools/prepare-worktrunk.py"
                    ):
                        raise ValueError("Worktrunk preparation pins differ")
                    relative = archive(source)
                    exclusions.extend(
                        "/" + str(path.relative_to(source))
                        for path in (source / ".tmp/worktrunk").glob(
                            "worktrunk-*.tar.xz"
                        )
                        if path.name != Path(relative).name
                    )
                else:
                    compatible(source, destination, group)
                    if group in {"node", "audit"}:
                        locks.enter_context(
                            cache_lock(source, METADATA / f"{group}.lock")
                        )
                    else:
                        found = False
                        for profile in ("debug", "release"):
                            if (source / "target" / profile).exists():
                                found = True
                                locks.enter_context(
                                    cache_lock(source, f"target/{profile}/.cargo-lock")
                                )
                        if not found:
                            raise ValueError("no compiled Rust cache")
                    compatible(source, destination, group)
                    for name in RUST_PAYLOAD if group == "rust" else roots:
                        safe_tree(canonical(source, name))
                selected.append(group)
            except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
                print(f"Skipping {group} cache: {error}")
                # Roots include individual files as well as directories.
                exclusions.extend(f"/{name}" for name in roots)
        if not selected:
            print(
                "No compatible cache available; normal make prepare/build remains available."
            )
            return
        command = ["wt", "step", "copy-ignored", "--require-include", "--from", branch]
        command += [
            "--config-set",
            "step.copy-ignored.exclude=" + json.dumps(exclusions),
        ]
        if dry_run:
            command.append("--dry-run")
        try:
            subprocess.run(command, cwd=destination, check=True)
            if not dry_run:
                for group in selected:
                    if group == "worktrunk":
                        archive(destination)
                        continue
                    compatible(source, destination, group)
                    if not all((destination / name).exists() for name in ROOTS[group]):
                        raise ValueError("selected cache was not copied")
                    if group == "audit" and digest(
                        destination / ROOTS[group][0]
                    ) != digest(source / ROOTS[group][0]):
                        raise ValueError("copied cargo-audit binary differs")
                    if group in {"node", "audit"}:
                        if group == "node" and digest(
                            source / "plugin/node_modules/.package-lock.json"
                        ) != digest(
                            destination / "plugin/node_modules/.package-lock.json"
                        ):
                            raise ValueError("copied npm lock differs")
                    record(destination, group)
        except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
            raise CopyError(
                "copy incomplete; run make prepare before building"
            ) from error


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    node = commands.add_parser("prepare-node")
    node.add_argument("--cold", action="store_true")
    audit = commands.add_parser("prepare-audit")
    audit.add_argument("--root", type=Path, required=True)
    stamp = commands.add_parser("record")
    stamp.add_argument("group", choices=["rust"])
    copier = commands.add_parser("copy")
    copier.add_argument("--from", dest="source", type=Path, required=True)
    copier.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    root = Path(output(["git", "rev-parse", "--show-toplevel"], Path.cwd())).resolve()
    if args.command == "prepare-node":
        prepare_node(root, args.cold)
    elif args.command == "prepare-audit":
        prepare_audit(root, args.root)
    elif args.command == "record":
        try:
            record(root, args.group)
        except (OSError, ValueError, subprocess.SubprocessError) as error:
            print(f"Not recording optional cache: {error}")
    else:
        try:
            copy(args.source, root, args.dry_run)
        except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
            # Cache reuse is optional, never a reason to prevent a cold checkout.
            print(f"Cache reuse unavailable: {error}. Use make prepare/build.")
        except CopyError as error:
            parser.exit(1, f"{error}\n")


if __name__ == "__main__":
    main()
