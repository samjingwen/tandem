#!/usr/bin/env python3
"""Exercise compatible copying with real disposable Git and Worktrunk consumers."""

import importlib.util
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

PROJECT = Path(__file__).resolve().parent.parent
SPEC = importlib.util.spec_from_file_location(
    "worktree_cache", PROJECT / "tools/worktree-cache.py"
)
CACHE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CACHE)


class CopyTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="tandem-cache-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.source = self.root / "source"
        self.source.mkdir()
        home = self.root / "home"
        home.mkdir()
        user = self.root / "user.toml"
        user.write_text("")
        self.env = {
            "PATH": os.environ["PATH"],
            "HOME": str(home),
            "WORKTRUNK_CONFIG_PATH": str(user),
            "WORKTRUNK_SYSTEM_CONFIG_PATH": str(self.root / "absent.toml"),
            "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_CONFIG_SYSTEM": os.devnull,
            "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_AUTHOR_NAME": "Cache fixture",
            "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
            "GIT_COMMITTER_NAME": "Cache fixture",
            "GIT_COMMITTER_EMAIL": "fixture@example.invalid",
            "npm_config_offline": "true",
            "npm_config_audit": "false",
            "npm_config_fund": "false",
        }
        self.command(["git", "init", "--initial-branch=source"])
        self.command(["git", "config", "core.hooksPath", os.devnull])
        for name in [
            "tools/worktree-cache.py",
            "tools/prepare-worktrunk.py",
            ".config/wt.toml",
            ".worktreeinclude",
            ".gitignore",
        ]:
            target = self.source / name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(PROJECT / name, target)
        (self.source / "plugin").mkdir()
        dependency = self.source / "dependency"
        dependency.mkdir()
        (dependency / "package.json").write_text(
            '{"name":"fixture-dep","version":"1.0.0","main":"index.js","bin":{"fixture-tool":"cli.js"}}'
        )
        (dependency / "index.js").write_text("module.exports = 'fixture';")
        (dependency / "cli.js").write_text(
            "#!/usr/bin/env node\nconsole.log('fixture');\n"
        )
        self.command(
            [
                "npm",
                "pack",
                "./dependency",
                "--pack-destination",
                "plugin",
                "--ignore-scripts",
                "--offline",
                "--json",
            ]
        )
        (self.source / "plugin/package.json").write_text(
            '{"name":"fixture","version":"1.0.0","private":true,"dependencies":{"fixture-dep":"file:fixture-dep-1.0.0.tgz"}}'
        )
        self.command(
            [
                "npm",
                "--prefix",
                "plugin",
                "install",
                "--package-lock-only",
                "--ignore-scripts",
                "--offline",
                "--no-audit",
                "--no-fund",
            ]
        )
        self.command(["git", "add", "."])
        self.command(["git", "commit", "-m", "fixture"])

    def command(self, command, cwd=None, check=True):
        return subprocess.run(
            command,
            cwd=cwd or self.source,
            env=self.env,
            capture_output=True,
            text=True,
            timeout=60,
            check=check,
        )

    def prepare(self):
        self.command([sys.executable, "tools/worktree-cache.py", "prepare-node"])
        modules = self.source / "plugin/node_modules"
        (modules / "cache-marker").write_text("source")

    def destination(self, hooks=False):
        destination = self.root / "destination"
        args = [
            "wt",
            "switch",
            "--create",
            "destination",
            "--base",
            "source",
            "--path",
            str(destination),
            "--no-cd",
            "--yes",
        ]
        if not hooks:
            args.append("--no-hooks")
        self.command(args)
        return destination

    def copy(self, destination, *args):
        return self.command(
            [
                sys.executable,
                "tools/worktree-cache.py",
                "copy",
                "--from",
                str(self.source),
                *args,
            ],
            destination,
        )

    def prepare_native_caches(self):
        # Synthetic native payloads exercise the real copy consumer, not compilers.
        for name in ["Cargo.toml", "Cargo.lock", "rust-toolchain.toml", "Makefile"]:
            shutil.copyfile(PROJECT / name, self.source / name)
        wrappers = self.root / "native-wrappers"
        wrappers.mkdir()
        for name in ["rustc", "cargo"]:
            executable = wrappers / name
            executable.write_text(f"#!/bin/sh\necho '{name} fixture-version'\n")
            executable.chmod(0o700)
        self.env["PATH"] = str(wrappers) + os.pathsep + self.env["PATH"]
        self.command(
            [
                "git",
                "add",
                "Cargo.toml",
                "Cargo.lock",
                "rust-toolchain.toml",
                "Makefile",
            ]
        )
        self.command(["git", "commit", "-m", "native fixture inputs"])
        for name in [
            "target/.rustc_info.json",
            "target/CACHEDIR.TAG",
            "target/debug/.cargo-lock",
            "target/debug/deps/cache.rlib",
            "target/debug/.fingerprint/cache/hash",
            "target/debug/build/cache/output",
            "target/debug/incremental/private-state",
            "target/debug/application",
            ".tmp/rust-tools/bin/cargo-audit",
            ".tmp/rust-tools/.crates.toml",
        ]:
            file = self.source / name
            file.parent.mkdir(parents=True, exist_ok=True)
            file.write_text("synthetic cache")
        self.command(
            [
                sys.executable,
                "-c",
                "import runpy; from pathlib import Path; m=runpy.run_path('tools/worktree-cache.py'); m['record'](Path.cwd(),'rust'); m['record'](Path.cwd(),'audit')",
            ]
        )
        (self.source / CACHE.METADATA / "audit.lock").touch()

    def test_native_files_and_nested_caches_are_really_copied(self):
        self.prepare_native_caches()
        destination = self.destination()
        preview = self.copy(destination, "--dry-run")
        self.assertIn("target/debug/deps", preview.stdout)
        self.assertIn(".tmp/rust-tools/bin/cargo-audit", preview.stdout)
        self.copy(destination)
        for name in [
            "target/.rustc_info.json",
            "target/debug/deps/cache.rlib",
            ".tmp/rust-tools/bin/cargo-audit",
        ]:
            self.assertEqual((destination / name).read_text(), "synthetic cache", name)
        for name in [
            "target/debug/.cargo-lock",
            "target/debug/incremental",
            "target/debug/application",
            ".tmp/rust-tools/.crates.toml",
        ]:
            self.assertFalse((destination / name).exists(), name)

    def test_busy_rust_cache_is_not_copied_but_audit_is(self):
        self.prepare_native_caches()
        destination = self.destination()
        with CACHE.cache_lock(self.source, "target/debug/.cargo-lock", exclusive=True):
            result = self.copy(destination)
        self.assertIn("Skipping rust", result.stdout)
        self.assertFalse((destination / "target").exists())
        self.assertTrue((destination / ".tmp/rust-tools/bin/cargo-audit").exists())

    def test_only_checksum_pinned_worktrunk_archive_is_copied(self):
        relative = CACHE.archive(PROJECT)
        archive = self.source / relative
        archive.parent.mkdir(parents=True)
        shutil.copyfile(PROJECT / relative, archive)
        (archive.parent / "worktrunk-untrusted.tar.xz").write_text("untrusted")
        destination = self.destination()
        preview = self.copy(destination, "--dry-run")
        self.assertIn(relative, preview.stdout)
        self.copy(destination)
        self.assertEqual(CACHE.digest(destination / relative), CACHE.digest(archive))
        self.assertFalse(
            (destination / ".tmp/worktrunk/worktrunk-untrusted.tar.xz").exists()
        )

    def test_real_copy_isolated_and_private_paths_excluded(self):
        self.prepare()
        for name in [
            ".env",
            ".env.local",
            ".tmp/python-tools/bin/python",
            ".tmp/runtime/store.db",
            ".tmp/retained-evidence/result.json",
        ]:
            file = self.source / name
            file.parent.mkdir(parents=True, exist_ok=True)
            file.write_text("synthetic private data")
        destination = self.destination()
        preview = self.copy(destination, "--dry-run")
        self.assertIn("plugin/node_modules", preview.stdout)
        self.assertFalse((destination / "plugin/node_modules").exists())
        self.copy(destination)
        copied = destination / "plugin/node_modules/cache-marker"
        self.assertEqual(copied.read_text(), "source")
        copied.write_text("destination")
        self.assertEqual(
            (self.source / "plugin/node_modules/cache-marker").read_text(), "source"
        )
        for name in [
            ".env",
            ".env.local",
            ".tmp/python-tools",
            ".tmp/runtime",
            ".tmp/retained-evidence",
        ]:
            self.assertFalse((destination / name).exists(), name)

    def test_private_entry_inside_cache_excludes_entire_cache(self):
        self.prepare()
        (self.source / "plugin/node_modules/.env").write_text("synthetic secret")
        destination = self.destination()
        result = self.copy(destination)
        self.assertIn("private/runtime/control entry", result.stdout)
        self.assertFalse((destination / "plugin/node_modules").exists())

    def test_changed_lockfile_skips_dependencies(self):
        self.prepare()
        destination = self.destination()
        (destination / "plugin/package-lock.json").write_text("{}")
        result = self.copy(destination)
        self.assertIn("Skipping node", result.stdout)
        self.assertFalse((destination / "plugin/node_modules").exists())

    def test_incompatible_groups_stay_excluded_when_another_group_copies(self):
        self.prepare()
        for name in [
            "target/debug/deps/cached.rlib",
            ".tmp/rust-tools/bin/cargo-audit",
        ]:
            file = self.source / name
            file.parent.mkdir(parents=True, exist_ok=True)
            file.write_text("synthetic incompatible cache")
        destination = self.destination()
        self.copy(destination)
        self.assertTrue((destination / "plugin/node_modules/cache-marker").exists())
        self.assertFalse((destination / "target").exists())
        self.assertFalse((destination / ".tmp/rust-tools").exists())

    def test_unknown_runtime_stamp_and_existing_cache_are_not_trusted(self):
        self.prepare()
        stamp = self.source / CACHE.METADATA / "node.json"
        value = json.loads(stamp.read_text())
        value["signature"]["platform"] = ["incompatible"]
        stamp.write_text(json.dumps(value))
        destination = self.destination()
        self.copy(destination)
        self.assertFalse((destination / "plugin/node_modules").exists())
        existing = destination / "plugin/node_modules/cache-marker"
        existing.parent.mkdir()
        existing.write_text("keep")
        self.copy(destination)
        self.assertEqual(existing.read_text(), "keep")

    def test_external_symlink_is_not_copied(self):
        self.prepare()
        (self.source / "plugin/node_modules/shared").symlink_to(self.root)
        destination = self.destination()
        result = self.copy(destination)
        self.assertIn("link outside", result.stdout)
        self.assertFalse((destination / "plugin/node_modules").exists())

    def test_absolute_internal_symlink_is_not_shared(self):
        self.prepare()
        modules = self.source / "plugin/node_modules"
        (modules / "shared").symlink_to(modules / "cache-marker")
        destination = self.destination()
        result = self.copy(destination)
        self.assertIn("absolute link", result.stdout)
        self.assertFalse((destination / "plugin/node_modules").exists())

    def test_relative_executable_links_remain_local_to_destination(self):
        self.prepare()
        destination = self.destination()
        self.copy(destination)
        executable = destination / "plugin/node_modules/.bin/fixture-tool"
        self.assertTrue(executable.is_symlink())
        self.assertFalse(Path(os.readlink(executable)).is_absolute())
        self.assertTrue(executable.resolve().is_relative_to(destination.resolve()))
        executable.write_text("destination only")
        self.assertNotEqual(
            (self.source / "plugin/node_modules/.bin/fixture-tool").read_text(),
            "destination only",
        )

    def test_busy_node_destination_is_not_mutated(self):
        self.prepare()
        destination = self.destination()
        with CACHE.cache_lock(
            destination, CACHE.METADATA / "node.lock", exclusive=True, create=True
        ):
            result = self.copy(destination)
        self.assertIn("Skipping node", result.stdout)
        self.assertFalse((destination / "plugin/node_modules").exists())

    def test_busy_audit_destination_is_not_mutated(self):
        self.prepare_native_caches()
        destination = self.destination()
        with CACHE.cache_lock(
            destination, CACHE.METADATA / "audit.lock", exclusive=True, create=True
        ):
            result = self.copy(destination)
        self.assertIn("Skipping audit", result.stdout)
        self.assertFalse((destination / ".tmp/rust-tools").exists())

    def test_preparation_normalizes_production_omit_and_executable_link_modes(self):
        package = self.source / "plugin/package.json"
        value = json.loads(package.read_text())
        value["devDependencies"] = value.pop("dependencies")
        package.write_text(json.dumps(value))
        self.command(
            [
                "npm",
                "--prefix",
                "plugin",
                "install",
                "--package-lock-only",
                "--ignore-scripts",
            ]
        )
        self.env.update(
            {
                "NODE_ENV": "production",
                "npm_config_omit": "dev",
                "npm_config_bin_links": "false",
            }
        )
        self.prepare()
        self.assertTrue((self.source / "plugin/node_modules/fixture-dep").is_dir())
        self.assertTrue(
            (self.source / "plugin/node_modules/.bin/fixture-tool").is_symlink()
        )
        for name in ["NODE_ENV", "npm_config_omit", "npm_config_bin_links"]:
            self.env.pop(name)
        result = self.command(
            [sys.executable, "tools/worktree-cache.py", "prepare-node"]
        )
        self.assertIn("Reusing compatible", result.stdout)

    def test_incompatible_and_untracked_audit_binaries_are_force_reinstalled(self):
        self.prepare_native_caches()
        cargo = self.root / "native-wrappers/cargo"
        cargo.write_text("""#!/bin/sh
if [ "$1" = --version ]; then echo 'cargo fixture-version'; exit; fi
case " $* " in *" --force "*) ;; *) exit 72 ;; esac
while [ "$#" -gt 0 ]; do
    if [ "$1" = --root ]; then shift; root=$1; fi
    shift
done
printf '#!/bin/sh\\necho cargo-audit 0.22.2\\n' > "$root/bin/cargo-audit"
chmod +x "$root/bin/cargo-audit"
""")
        self.env["RUSTFLAGS"] = "--cfg=new-toolchain-settings"
        args = [sys.executable, "tools/worktree-cache.py", "prepare-audit", "--root"]
        self.command([*args, str(self.source / ".tmp/rust-tools")])
        destination = self.destination()
        self.copy(destination)
        self.assertFalse((destination / ".tmp/rust-tools/.crates.toml").exists())
        self.env["RUSTFLAGS"] = "--cfg=changed-again"
        self.command([*args, str(destination / ".tmp/rust-tools")], destination)
        self.assertTrue((destination / CACHE.METADATA / "audit.json").exists())

    def test_in_use_node_cache_is_skipped(self):
        self.prepare()
        destination = self.destination()
        with CACHE.cache_lock(
            self.source, CACHE.METADATA / "node.lock", exclusive=True
        ):
            result = self.copy(destination)
        self.assertIn("Skipping node", result.stdout)
        self.assertFalse((destination / "plugin/node_modules").exists())

    def test_pre_start_finishes_copy_before_execute(self):
        self.prepare()
        destination = self.root / "destination"
        result = self.command(
            [
                "wt",
                "switch",
                "--create",
                "destination",
                "--base",
                "source",
                "--path",
                str(destination),
                "--no-cd",
                "--yes",
                "--execute",
                sys.executable,
                "--",
                "-c",
                "from pathlib import Path; assert Path('plugin/node_modules/cache-marker').read_text() == 'source'; print('execute-after-copy')",
            ]
        )
        self.assertIn("execute-after-copy", result.stdout)

    def test_missing_include_is_noop_in_real_consumer(self):
        self.prepare()
        destination = self.destination()
        (self.source / ".worktreeinclude").unlink()
        self.command(
            ["wt", "step", "copy-ignored", "--from", "source", "--require-include"],
            destination,
        )
        self.assertFalse((destination / "plugin/node_modules").exists())

    def test_warm_preparation_does_not_install_and_cold_path_still_installs(self):
        self.prepare()
        version = self.command(["npm", "--version"]).stdout.strip()
        wrapper = self.root / "wrappers"
        wrapper.mkdir()
        npm = wrapper / "npm"
        npm.write_text(
            f'#!/bin/sh\nif [ "$1" = "--version" ]; then echo "{version}"; else exit 73; fi\n'
        )
        npm.chmod(0o700)
        self.env["PATH"] = str(wrapper) + os.pathsep + self.env["PATH"]
        command = [sys.executable, "tools/worktree-cache.py", "prepare-node"]
        warm = self.command(command)
        self.assertIn("Reusing compatible", warm.stdout)
        cold = self.command([*command, "--cold"], check=False)
        self.assertNotEqual(cold.returncode, 0)
        self.assertIn("73", cold.stderr)
        self.assertFalse((self.source / CACHE.METADATA / "node.json").exists())

    def test_dependency_preparation_does_not_require_git_ownership_trust(self):
        self.env["GIT_TEST_ASSUME_DIFFERENT_OWNER"] = "1"
        result = self.command(["git", "rev-parse", "--show-toplevel"], check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("dubious ownership", result.stderr)
        self.prepare()
        self.assertTrue((self.source / CACHE.METADATA / "node.json").exists())


if __name__ == "__main__":
    unittest.main()
