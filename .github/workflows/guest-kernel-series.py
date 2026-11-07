"""Check the unmodified packaged inputs and the original ARM64 Go owner."""

import hashlib
import json
import os
import platform
import subprocess
import sys
import xml.etree.ElementTree as ET
from pathlib import Path
from urllib.parse import unquote, urlsplit

work = Path(sys.argv[1])
result = {"source": os.environ["PRODUCT"], "machine": platform.machine(),
          "packages": {}, "owners": {}}


def save():
    (work / "result.json").write_text(json.dumps(result, sort_keys=True, indent=2) + "\n")


def bazel(command, *args):
    return subprocess.run([work / "bazel", command, "--bes_backend=", "--jobs=3", *args]).returncode


save()
pins = {
    "arm64": {"vmlinux": ("5.10", "957d6da5c7ee7869c8a285784a201ac3928393087e2542131d4d107fc9556b45")},
    "x86_64": {"vmlinux": ("5.15", "c6dd56fff7dada624dcb68db317541f45504261ae6f2aae5dc91848aacbbfda3"),
               "vmlinux_6.18": ("6.18", "757012f067c4db854f2a9bea6d76ce30242480c621114da8c07055bf1735819e")},
}
for arch, artifacts in pins.items():
    exit_code = bazel("build", "--platforms=//platforms:linux_" + arch,
                      *["//enterprise/vmsupport/bin:" + name for name in artifacts])
    package = result["packages"][arch] = {"exit": exit_code, "images": {}}
    if exit_code == 0:
        for filename, (series, expected) in artifacts.items():
            image = (Path("bazel-bin/enterprise/vmsupport/bin") / filename).read_bytes()
            digest = hashlib.sha256(image).hexdigest()
            package["images"][series] = {"sha256": digest, "pinMatches": digest == expected,
                                          "bannerMatches": b"Linux version " + series.encode() + b"." in image}
    save()

targets = {
    "//enterprise/server/remote_execution/containers/firecracker:guest_kernel_test":
        ["TestPackagedGuestKernelSeries/5.10", "TestPackagedGuestKernelSeries/5.15",
         "TestPackagedGuestKernelSeries/6.18", "TestGuestKernelBootAndSnapshot/5.15",
         "TestGuestKernelBootAndSnapshot/6.18"],
    "//server/util/platform:platform_test": ["TestParse_GuestKernelVersion/5.10"],
}
bep = work / "test.bep"
result["testExit"] = bazel("test", "--platforms=//platforms:linux_arm64",
                          "--nocache_test_results", "--test_output=errors",
                          "--test_env=GO_TEST_WRAP_TESTV=1", "--build_event_json_file=" + str(bep),
                          *targets)
save()
if bep.is_file():
    for line in bep.open():
        event = json.loads(line)
        key = event.get("id", {}).get("testResult", {})
        target = key.get("label")
        if target not in targets:
            continue
        owner = result["owners"].setdefault(target, [])
        if "testResult" not in event:
            owner.append({"status": "ABORTED", "reason": event.get("aborted", {}).get("reason", "UNKNOWN")})
            save()
            continue
        test = event["testResult"]
        outcome = {"status": test["status"], "cases": {}}
        owner.append(outcome)
        pkg, name = target.removeprefix("//").split(":")
        xml = Path("bazel-testlogs") / pkg / name / "test.xml"
        if not xml.is_file():
            continue
        data = xml.read_bytes()
        sha = hashlib.sha256(data).hexdigest()
        outcome["xmlSHA256"] = sha
        outcome["xmlMatches"] = False
        for output in test.get("testActionOutput", []):
            if output.get("name") != "test.xml":
                continue
            uri = urlsplit(output.get("uri", ""))
            if output.get("digest"):
                outcome["xmlMatches"] = output["digest"] == sha
            elif uri.scheme == "file" and not uri.netloc:
                outcome["xmlMatches"] = Path(unquote(uri.path)).resolve() == xml.resolve()
        for executable in (Path("bazel-bin") / pkg / (name + "_") / name,
                           Path("bazel-bin") / pkg / name):
            if executable.is_file():
                with executable.open("rb") as f:
                    header = f.read(20)
                if header[:6] == b"\x7fELF\x02\x01":
                    outcome["elfMachine"] = int.from_bytes(header[18:20], "little")
                    break
        if not outcome["xmlMatches"]:
            continue
        for case in ET.fromstring(data).iter("testcase"):
            name = case.get("name", "")
            for expected in targets[target]:
                if name == expected or name.endswith(("/" + expected, "." + expected)):
                    outcome["cases"][expected] = not any(case.find(tag) is not None for tag in ("skipped", "error", "failure"))
        save()
save()
assert result["testExit"] == 0
for arch, artifacts in pins.items():
    package = result["packages"][arch]
    assert package["exit"] == 0 and len(package["images"]) == len(artifacts)
    assert all(i["pinMatches"] and i["bannerMatches"] for i in package["images"].values())
for target, required in targets.items():
    owner, = result["owners"][target]
    assert owner["status"] == "PASSED" and owner["xmlMatches"]
    assert owner.get("elfMachine") == 183
    assert all(owner["cases"].get(name) for name in required)
