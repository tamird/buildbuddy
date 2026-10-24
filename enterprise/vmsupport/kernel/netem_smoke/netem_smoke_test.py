"""Boot the retained changed kernel with declared QEMU TCG and real netem."""
import hashlib
import json
import os
import posixpath
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest

TOOLS, KERNEL, INIT = (Path(arg).resolve() for arg in sys.argv[1:])
EXPECTED_KERNEL = "15e5a8edd2f5786019408d38d68084c5bd92cf6f7c44a186c2adaf1e1bf3e532"


class NetemSmokeTest(unittest.TestCase):
    def test_changed_kernel_netem(self) -> None:
        self.assertEqual(hashlib.sha256(KERNEL.read_bytes()).hexdigest(), EXPECTED_KERNEL)
        self.assertEqual(KERNEL.stat().st_size, 33762240)
        output = Path(os.environ["TEST_UNDECLARED_OUTPUTS_DIR"])
        output.mkdir(parents=True, exist_ok=True)
        print("NETEM_KERNEL sha256=" + EXPECTED_KERNEL + " bytes=33762240")
        with tempfile.TemporaryDirectory(dir=os.environ["TEST_TMPDIR"]) as temporary:
            root = Path(temporary)
            tools = root / "tools"
            tools.mkdir()
            # The archive is the declared, snapshot-pinned APT closure.
            with tarfile.open(TOOLS) as archive:
                for member in archive:
                    name = posixpath.normpath(member.name)
                    self.assertFalse(name.startswith("/") or name == ".." or name.startswith("../"), name)
                    self.assertTrue(member.isfile() or member.isdir() or member.issym() or member.islnk(), name)
                    if member.issym() or member.islnk():
                        link = member.linkname
                        target = posixpath.normpath(posixpath.join(posixpath.dirname(name) if member.issym() else "", link))
                        self.assertFalse(link.startswith("/") or target == ".." or target.startswith("../"), (name, link))
                archive.extractall(tools)
            loader = tools / "usr/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2"
            libraries = str(tools / "usr/lib/x86_64-linux-gnu")
            def command(path: str) -> list[str]:
                return [str(loader), "--library-path", libraries, str(tools / path)]
            for name, path in (("qemu", "usr/bin/qemu-system-x86_64"), ("cpio", "usr/bin/cpio")):
                listed = subprocess.check_output(command(path)[:3] + ["--list", command(path)[3]], text=True, timeout=10)
                (output / (name + "-libraries.txt")).write_text(listed)
                for library in re.findall(r"(?:=>\s+)?(/[^\s]+)\s+\(", listed):
                    self.assertTrue(Path(library).resolve().is_relative_to(tools), library)
            guest = root / "guest"
            (guest / "usr/bin").mkdir(parents=True)
            (guest / "usr/sbin").mkdir()
            (guest / "usr/lib").mkdir()
            shutil.copytree(tools / "usr/lib/x86_64-linux-gnu", guest / "usr/lib/x86_64-linux-gnu", symlinks=True)
            (guest / "usr/lib64").mkdir()
            (guest / "usr/lib64/ld-linux-x86-64.so.2").symlink_to("../lib/x86_64-linux-gnu/ld-linux-x86-64.so.2")
            for name in ("bin", "sbin", "lib", "lib64"):
                (guest / name).symlink_to("usr/" + name)
            for path in ("usr/bin/busybox", "usr/sbin/ip", "usr/sbin/tc"):
                shutil.copy2(tools / path, guest / path)
            shutil.copy2(INIT, guest / "init")
            (guest / "init").chmod(0o755)
            members = sorted(str(path.relative_to(guest)) for path in guest.rglob("*"))
            initrd = root / "initrd.cpio"
            with initrd.open("wb") as stream:
                subprocess.run(command("usr/bin/cpio") + ["--null", "-o", "-H", "newc", "--quiet", "--owner=0:0"],
                               cwd=guest, input="\0".join(members).encode() + b"\0", stdout=stream, check=True, timeout=30)
            qemu = command("usr/bin/qemu-system-x86_64")
            environment = {**os.environ, "QEMU_MODULE_DIR": str(tools / "usr/lib/x86_64-linux-gnu/qemu")}
            version = subprocess.check_output(qemu + ["--version"], text=True, timeout=10, env=environment)
            argv = qemu + ["-machine", "pc", "-accel", "tcg,thread=single", "-cpu", "max", "-smp", "1", "-m", "512",
                           "-L", str(tools / "usr/share/qemu"), "-bios", str(tools / "usr/share/seabios/bios-256k.bin"),
                           "-nodefaults", "-no-user-config", "-display", "none", "-monitor", "none", "-serial", "stdio", "-no-reboot",
                           "-kernel", str(KERNEL), "-initrd", str(initrd), "-append", "console=ttyS0 panic=-1 rdinit=/init"]
            (output / "command.json").write_text(json.dumps(argv, indent=2) + "\n")
            (output / "qemu-version.txt").write_text(version)
            (output / "identities.json").write_text(json.dumps({
                "kernel": {"sha256": EXPECTED_KERNEL, "bytes": KERNEL.stat().st_size},
                "tools": {"sha256": hashlib.sha256(TOOLS.read_bytes()).hexdigest(), "bytes": TOOLS.stat().st_size},
                "emulator": "QEMU TCG", "initrd_sha256": hashlib.sha256(initrd.read_bytes()).hexdigest(),
            }, indent=2) + "\n")
            with (output / "guest.log").open("wb") as log:
                completed = subprocess.run(argv, stdout=log, stderr=subprocess.STDOUT, timeout=180, env=environment)
            log = (output / "guest.log").read_text(errors="replace")
            print(version, log, sep="\n")
            self.assertEqual(completed.returncode, 0)
            self.assertEqual(re.findall(r"NETEM_EXIT=([0-9]+)", log), ["0"])
            self.assertRegex(log, r"NETEM_ADDED\r?\nqdisc netem .*limit 100 delay 50ms rate 20Mbit")
            self.assertRegex(log, r"NETEM_CHANGED\r?\nqdisc netem .*limit 100 delay 5ms rate 20Mbit")
            self.assertIn("NETEM_CLEANED", log)


if __name__ == "__main__":
    unittest.main(argv=[sys.argv[0]], verbosity=2)
