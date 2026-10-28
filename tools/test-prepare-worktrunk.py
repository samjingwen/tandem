#!/usr/bin/env python3
"""Offline behavioral checks of the real checksum/extraction/install consumer."""

import hashlib
import importlib.util
import io
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location(
    "prepare_worktrunk", Path(__file__).with_name("prepare-worktrunk.py")
)
installer = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(installer)


def archive_for(binary, *, kind=tarfile.REGTYPE, name="worktrunk-fixture/wt"):
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w:xz") as archive:
        member = tarfile.TarInfo(name)
        member.type = kind
        member.size = len(binary) if kind == tarfile.REGTYPE else 0
        member.linkname = "../../escape"
        archive.addfile(member, io.BytesIO(binary))
        # A release contains other files. None may be extracted, even if hostile.
        other = tarfile.TarInfo("../must-not-extract")
        other.size = 7
        archive.addfile(other, io.BytesIO(b"ignored"))
    return output.getvalue()


class PreparationTests(unittest.TestCase):
    def test_verified_install_repair_and_cache_execute_real_binary(self):
        binary = b"#!/bin/sh\nprintf 'wt v0.81.0\\n'\n"
        data = archive_for(binary)
        digest = hashlib.sha256(data).hexdigest()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve() / "tools"
            with (
                patch.dict(
                    installer.ARCHIVES, {("Fixture", "cpu"): ("fixture", digest)}
                ),
                patch.object(installer.platform, "system", return_value="Fixture"),
                patch.object(installer.platform, "machine", return_value="cpu"),
                patch.object(installer.urllib.request, "urlopen") as download,
            ):
                download.return_value.__enter__.return_value = io.BytesIO(data)
                installer.prepare(root)
                download.assert_called_once_with(
                    "https://github.com/max-sixty/worktrunk/releases/download/v0.81.0/worktrunk-fixture.tar.xz",
                    timeout=30,
                )
                executable = root / "bin/wt"
                self.assertEqual(executable.read_bytes(), binary)
                self.assertEqual(executable.stat().st_mode & 0o777, 0o700)
                self.assertFalse((root.parent / "must-not-extract").exists())
                executable.write_text("tampered executable")
                download.reset_mock()
                installer.prepare(root)
                download.assert_not_called()
                self.assertEqual(executable.read_bytes(), binary)
                (root / "worktrunk-fixture.tar.xz").write_bytes(b"bad cache")
                with self.assertRaisesRegex(ValueError, "checksum"):
                    installer.prepare(root)
                self.assertEqual(executable.read_bytes(), binary)

    def test_bad_archives_are_refused_before_any_execution(self):
        data = archive_for(b"fixture")
        for digest in ["0" * 64, hashlib.sha256(data[:-1]).hexdigest()]:
            with self.assertRaisesRegex(ValueError, "checksum"):
                installer.binary_from_archive(data, digest, "fixture")
        for kind in [tarfile.SYMTYPE, tarfile.LNKTYPE, tarfile.DIRTYPE]:
            data = archive_for(b"", kind=kind)
            with self.assertRaisesRegex(ValueError, "regular"):
                installer.binary_from_archive(
                    data, hashlib.sha256(data).hexdigest(), "fixture"
                )
        data = archive_for(b"", name="wrong/wt")
        with self.assertRaisesRegex(ValueError, "exactly one"):
            installer.binary_from_archive(
                data, hashlib.sha256(data).hexdigest(), "fixture"
            )

    def test_install_does_not_follow_symlinks_or_accept_wrong_version(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            outside = root / "outside"
            outside.write_text("preserve")
            link = root / "wt"
            link.symlink_to(outside)
            with self.assertRaisesRegex(ValueError, "nonregular"):
                installer.atomic_write(link, b"changed", 0o700)
            self.assertEqual(outside.read_text(), "preserve")
            parent = root / "alias"
            parent.symlink_to(root, target_is_directory=True)
            with self.assertRaisesRegex(ValueError, "canonical"):
                installer.prepare(parent / "new-tools")
            data = archive_for(b"#!/bin/sh\necho wt v0.0.0\n")
            digest = hashlib.sha256(data).hexdigest()
            target, _ = installer.ARCHIVES[
                (installer.platform.system(), installer.platform.machine())
            ]
            # Use the normal entry point, but an offline pinned fixture asset.
            data = archive_for(
                b"#!/bin/sh\necho wt v0.0.0\n", name=f"worktrunk-{target}/wt"
            )
            digest = hashlib.sha256(data).hexdigest()
            with (
                patch.dict(
                    installer.ARCHIVES,
                    {
                        (installer.platform.system(), installer.platform.machine()): (
                            target,
                            digest,
                        )
                    },
                ),
                patch.object(installer.urllib.request, "urlopen") as download,
            ):
                download.return_value.__enter__.return_value = io.BytesIO(data)
                with self.assertRaisesRegex(ValueError, "version"):
                    installer.prepare(root / "wrong-version")


if __name__ == "__main__":
    unittest.main()
