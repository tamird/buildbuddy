"""Qualify the existing kernel build targets and one changed-guest smoke."""
import datetime
import hashlib
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tempfile
import tarfile
import time
import re
import xml.etree.ElementTree as ET
from urllib.parse import unquote, urlsplit
import urllib.request

import credential_artifacts
from image_execution import execution_records

SOURCE = os.environ["NETEM_QUALIFICATION_SOURCE"]
KERNELS = ["//enterprise/vmsupport/kernel:guest_kernel_" + name for name in
           ("x86_64_v5_15", "x86_64_v6_18", "aarch64_v5_10")]
TCG_SMOKE = "//enterprise/vmsupport/kernel/netem_smoke:netem_smoke_test"
SMOKE = "//enterprise/server/remote_execution/containers/firecracker:firecracker_test"
REPORTER_SOURCE = "462126434f0cdf5e56f37603c82b563b456f1ce8"
REPORTER_HASHES = {
    "qualification_report.py": "3929b631d7d28c9dd97720281d8629af7a6bb611d670dafaf96793a4a8652e83",
    "unit_matrix.py": "209f47696a8c28e0072a6199d40f85edb855e656195189741ba4cb6215246a37",
}


def main() -> int:
    if sys.version_info < (3, 11):
        raise RuntimeError("Qualification requires Python 3.11+")
    if sys.argv[1:] not in ([], ["--prepare-lock"], ["--tcg-smoke"]):
        raise ValueError("Expected no arguments, --prepare-lock, or --tcg-smoke")
    prepare_lock = sys.argv[1:] == ["--prepare-lock"]
    tcg_smoke = sys.argv[1:] == ["--tcg-smoke"]
    work_seconds = 900 if prepare_lock or tcg_smoke else 2400
    workspace = Path.cwd()
    if subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip() != SOURCE:
        raise RuntimeError("Unexpected source")
    if subprocess.check_output(["git", "status", "--porcelain", "--untracked-files=no"]):
        raise RuntimeError("Dirty tracked source")
    if (workspace / "user.bazelrc").exists():
        raise RuntimeError("Unexpected per-user workspace RC")
    artifacts = Path(os.environ["BUILDBUDDY_ARTIFACTS_DIRECTORY"])
    artifacts.mkdir(parents=True, exist_ok=True)
    deadline = time.monotonic() + work_seconds
    runner_exit = capture_exit = 0
    with tempfile.TemporaryDirectory(prefix="netem-qualification-") as temporary:
        private = Path(temporary)
        auth = private / "auth"
        auth.mkdir(mode=0o700)
        credential_artifacts.prepare(auth)
        names = json.loads(os.environ.get("BUILDBUDDY_SECRET_ENV_VAR_NAMES", "[]"))
        if not isinstance(names, list) or any(not isinstance(name, str) for name in names):
            raise ValueError("Invalid runner secret inventory")
        names += ["BUILDBUDDY_API_KEY", "BB_GRPC_CLIENT_IDENTITY", "REPO_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"]
        secrets = {os.environ.pop(name).encode() for name in names if os.environ.get(name)}
        secrets |= {json.dumps(value.decode())[1:-1].encode() for value in secrets}
        # The supported hosted wrapper declares its native Bazel binary here.
        # Its generated RC embeds header credentials, so use the same binary
        # with the normal workspace RC and the private native helper instead.
        wrapper = Path(shutil.which("bazel") or "")
        tokens = shlex.split(wrapper.read_text(), comments=True)
        candidates = [token.removeprefix("BAZEL_BIN=") for token in tokens if token.startswith("BAZEL_BIN=")]
        if len(candidates) != 1 or not Path(candidates[0]).is_file():
            raise ValueError("Hosted wrapper omitted its native Bazel binary")
        bazel = candidates[0]
        # Read only the UUID metadata; never copy the credential-bearing RC.
        runner_rc = Path(os.environ["BUILDBUDDY_CI_RUNNER_ROOT_DIR"]) / "buildbuddy.bazelrc"
        parent_ids = re.findall(
            r"^common --build_metadata=PARENT_INVOCATION_ID=([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$",
            runner_rc.read_text(), re.MULTILINE)
        if len(parent_ids) != 1:
            raise ValueError("Hosted runner omitted its unique parent invocation UUID")
        rc = private / "auth.bazelrc"
        rc.write_text("common --credential_helper=remote.buildbuddy.io=" + str(auth / "helper") + "\n")
        startup = ["--nosystem_rc", "--nohome_rc", "--bazelrc=" + str(rc), "--digest_function=sha256"]
        options = ["--config=remote", "--config=target-linux-x86-exec-multiarch",
                   "--remote_executor=grpcs://remote.buildbuddy.io", "--remote_cache=grpcs://remote.buildbuddy.io",
                   "--experimental_remote_downloader=grpcs://remote.buildbuddy.io", "--bes_backend=grpcs://remote.buildbuddy.io",
                   "--bes_results_url=https://app.buildbuddy.io/invocation/", "--remote_download_outputs=all",
                   "--lockfile_mode=error", "--noannounce_rc", "--execution_log_compact_file=",
                   "--build_event_json_file_path_conversion=false", "--remote_timeout=15m",
                   "--build_metadata=PARENT_INVOCATION_ID=" + parent_ids[0],
                   "--build_metadata=COMMIT_SHA=" + SOURCE,
                   "--build_metadata=REPO_URL=https://github.com/tamird/buildbuddy"]
        for name, expected in REPORTER_HASHES.items():
            with urllib.request.urlopen(f"https://raw.githubusercontent.com/tamird/gvisor/{REPORTER_SOURCE}/test/rbe/{name}", timeout=30) as response:
                raw = response.read(2 * 1024 * 1024 + 1)
            if hashlib.sha256(raw).hexdigest() != expected:
                raise ValueError("Pinned qualification helper identity differs")
            (private / name).write_bytes(raw)
        sys.path.insert(0, str(private))
        import qualification_report as report
        evidence = private / "evidence"
        evidence.mkdir()
        report.write_json(evidence / "plan.json", {"source": SOURCE, "kernels": KERNELS, "smoke": SMOKE,
                          "work_seconds": work_seconds, "prepare_lock": prepare_lock, "tcg_smoke": tcg_smoke, "native_bazel": bazel, "reporter": REPORTER_HASHES,
                          "scope": "Canonical dependency lock preparation only; no builds or tests." if prepare_lock else
                                   ("One exact retained-kernel QEMU TCG smoke; no kernel rebuild or Firecracker credit." if tcg_smoke else
                                    "Three canonical builds; one uncached AMD64 changed-kernel bare-pool smoke. No uploads or deployment.")})
        output_root = None

        def clean(data: bytes) -> bytes:
            for value in secrets:
                data = data.replace(value, b"[REDACTED]")
            return data

        def invoke(name: str, args: list[str], kind: str, targets: list[str], *, retain_updated_lock: bool = False) -> dict:
            nonlocal runner_exit, capture_exit, output_root
            directory = evidence / ("invocation-" + name)
            directory.mkdir()
            events = directory / "events.jsonl"
            raw_events = private / (name + ".events.jsonl")
            command_options = [] if retain_updated_lock else options
            command = [bazel, *startup, args[0], *command_options, *args[1:]]
            if kind != "command":
                command += ["--build_event_json_file=" + str(raw_events)]
            report.write_json(directory / "command.json", command)
            (directory / "arguments.txt").write_text(json.dumps(command[1:]) if kind == "command" else "\n".join(command[1:]) + "\n")
            (directory / "expected-targets.txt").write_text("".join(label + "\n" for label in targets))
            for name, git_args in (("commit.txt", ["cat-file", "commit", SOURCE]), ("before.txt", ["status", "--porcelain", "--untracked-files=no"])):
                (directory / name).write_bytes(subprocess.check_output(["git", *git_args], timeout=30))
            (directory / "lock-before").write_bytes((workspace / "MODULE.bazel.lock").read_bytes())
            native = None
            captured = 0
            try:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError("Qualification work deadline exhausted")
                with (directory / "stdout.log").open("wb") as stdout, (directory / "stderr.log").open("wb") as stderr:
                    native = subprocess.run(["timeout", "--signal=TERM", "--kill-after=5s", str(remaining), *command], stdout=stdout, stderr=stderr).returncode
            finally:
                if retain_updated_lock:
                    # Retain the generated dependency result, then restore the
                    # exact original before the normal source-integrity check.
                    try:
                        (evidence / "MODULE.bazel.lock").write_bytes((workspace / "MODULE.bazel.lock").read_bytes())
                    finally:
                        (workspace / "MODULE.bazel.lock").write_bytes((directory / "lock-before").read_bytes())
                finished = datetime.datetime.now(datetime.timezone.utc).isoformat()
                if native is not None:
                    (directory / "native-exit").write_text(str(native) + "\n")
                runner_exit = runner_exit or (native if native is not None else 1)
                for filename in ("stdout.log", "stderr.log"):
                    path = directory / filename
                    if path.exists():
                        path.write_bytes(clean(path.read_bytes()))
                try:
                    for name, git_args in (("after.txt", ["status", "--porcelain", "--untracked-files=no"]), ("head.txt", ["rev-parse", "HEAD"])):
                        (directory / name).write_bytes(subprocess.check_output(["git", *git_args], timeout=30))
                    (directory / "lock-after").write_bytes((workspace / "MODULE.bazel.lock").read_bytes())
                    if kind != "command":
                        result = report.capture(raw_events, events, output_root, directory / "reports")
                        captured = int(bool(result["errors"]))
                except (OSError, ValueError, subprocess.SubprocessError) as error:
                    report.write_json(directory / "capture-error.json", {"error_type": type(error).__name__})
                    captured = 1
                capture_exit = capture_exit or captured
                (directory / "capture-exit").write_text(str(captured) + "\n")
                report.write_json(directory / "context.json", {
                    "expected_source": SOURCE, "command_kind": kind, "execution_finished_at": finished,
                    "commit_file": "commit.txt", "source_before": "before.txt", "source_after": "after.txt",
                    "final_head": "head.txt", "executed_arguments": "arguments.txt", "native_exit": "native-exit",
                    "capture_exit": "capture-exit", "lock_before": "lock-before", "lock_after": "lock-after"})
                if kind == "command":
                    verdict = report.command_verdict(directory / "context.json")
                else:
                    verdict = report.verdict(events, report.manifest_path(events), directory / "context.json", directory / "expected-targets.txt")
                report.write_json(directory / "verdict.json", verdict)
                print(json.dumps({"phase": directory.name, "native_exit": native, "status": verdict["status"]}), flush=True)
            if verdict["status"] != "PASS":
                raise RuntimeError("Qualification phase did not pass: " + directory.name)
            return verdict

        retained_kernel = workspace / "enterprise/vmsupport/kernel/netem_smoke/retained-vmlinux"
        staged_kernel = False

        def fetch_retained(item: dict, path: Path) -> None:
            # Reuse the existing retained-runtime GetFile boundary. The private
            # helper owns the credential, which is never restored to the env.
            key, = json.loads((auth / "helper.json").read_text())["headers"]["x-buildbuddy-api-key"]
            request = urllib.request.Request("https://app.buildbuddy.io/api/v1/GetFile",
                data=json.dumps({"uri": item["uri"]}).encode(),
                headers={"Content-Type": "application/json", "x-buildbuddy-api-key": key})
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("Retained kernel retrieval exhausted the work deadline")
            with urllib.request.urlopen(request, timeout=min(180, remaining)) as response:
                data = response.read(item["bytes"] + 1)
            if len(data) != item["bytes"] or hashlib.sha256(data).hexdigest() != item["sha256"]:
                raise ValueError("Retained kernel supplier identity differs")
            path.write_bytes(data)

        completed = False
        try:
            if prepare_lock:
                invoke("dependencies", ["mod", "deps", "--lockfile_mode=update"], "command", [], retain_updated_lock=True)
            elif tcg_smoke:
                if retained_kernel.exists() or retained_kernel.is_symlink():
                    raise ValueError("Retained kernel destination already exists")
                supplier = {"uri": "bytestream://remote.buildbuddy.io/bb-snapshot/blobs/8a7ac41432c0c46dcfa095e2076f4d9350d838702fd8cf20588094e41f7e3f8d/551950",
                            "bytes": 551950, "sha256": "8a7ac41432c0c46dcfa095e2076f4d9350d838702fd8cf20588094e41f7e3f8d"}
                supplier_path = private / "kernel-supplier.tar.gz"
                fetch_retained(supplier, supplier_path)
                with tarfile.open(supplier_path) as archive:
                    verdict = json.load(archive.extractfile("invocation-kernels/verdict.json"))
                    if verdict["source"] != "8c4bcd313e750e1e29828c79e831923ad7f15ca1" or verdict["status"] != "PASS" or verdict["native_exit"] != 0:
                        raise ValueError("Retained kernel supplier build did not pass")
                    events = evidence / "kernel-supplier-events.jsonl"
                    events.write_bytes(archive.extractfile("invocation-kernels/events.jsonl").read())
                    report.write_json(evidence / "kernel-supplier-verdict.json", verdict)
                groups = {(report.Owner(row["label"], row["configuration"]), "default") for row in verdict["expected"]}
                outputs = report.advertised_build_outputs(events, groups)
                kernel, = [row for row in outputs if row["identity"]["label"] == KERNELS[1]]
                if kernel["sha256"] != "15e5a8edd2f5786019408d38d68084c5bd92cf6f7c44a186c2adaf1e1bf3e532" or kernel["bytes"] != 33762240:
                    raise ValueError("Unexpected retained 6.18 kernel")
                blob = {**kernel, "uri": f"bytestream://remote.buildbuddy.io/blobs/{kernel['sha256']}/{kernel['bytes']}"}
                fetch_retained(blob, retained_kernel)
                staged_kernel = True
                report.write_json(evidence / "kernel-supplier.json", {"bundle": supplier, "output": kernel, "materialized": report.file_record(retained_kernel)})
                invoke("execution-root", ["info", "output_base"], "command", [])
                output_root = Path((evidence / "invocation-execution-root/stdout.log").read_text().strip())
                if not output_root.is_absolute() or not output_root.is_dir():
                    raise ValueError("Invalid native output base")
                invoke("buildifier", ["run", "//:buildifier", "--", "-mode=check", "enterprise/vmsupport/kernel/netem_smoke/BUILD", "deps/apt.MODULE.bazel"], "command", [])
                invoke("tcg-smoke", ["test", "--test_sharding_strategy=disabled", "--nocache_test_results", "--runs_per_test=1", "--flaky_test_attempts=1", "--test_timeout=300", TCG_SMOKE], "test", [TCG_SMOKE])
                manifest = report.read_json(report.manifest_path(evidence / "invocation-tcg-smoke/events.jsonl"))
                logs = [row for row in manifest["reports"] if row["name"] == "test.log"]
                if len(logs) != 1:
                    raise ValueError("Expected one TCG smoke log")
                log = (evidence / "invocation-tcg-smoke" / logs[0]["path"]).read_text()
                if any(marker not in log for marker in ("test_changed_kernel_netem", "Ran 1 test", "NETEM_EXIT=0", "NETEM_ADDED", "NETEM_CHANGED", "NETEM_CLEANED", "NETEM_KERNEL sha256=" + kernel["sha256"] + " bytes=" + str(kernel["bytes"]))):
                    raise ValueError("TCG smoke omitted a required native marker")
                report.write_json(evidence / "tcg-smoke.json", {"status": "PASS", "kernel": kernel, "log": logs[0], "scope": "QEMU TCG boot and actual qdisc control; no Firecracker or packet-rate measurement."})
            else:
                invoke("execution-root", ["info", "output_base"], "command", [])
                output_root = Path((evidence / "invocation-execution-root/stdout.log").read_text().strip())
                if not output_root.is_absolute() or not output_root.is_dir():
                    raise ValueError("Invalid native output base")
                invoke("buildifier", ["run", "//:buildifier", "--", "-mode=check", "enterprise/server/remote_execution/containers/firecracker/BUILD"], "command", [])
                invoke("gofmt", ["run", "//:gofmt", "--", "-d", "enterprise/server/remote_execution/containers/firecracker/netem_qualification_test.go"], "command", [])
                if (evidence / "invocation-gofmt/stdout.log").read_bytes():
                    raise ValueError("gofmt emitted a diff")
                action_log = private / "kernel-actions.json"
                built = invoke("kernels", ["build", "--noremote_accept_cached", "--execution_log_json_file=" + str(action_log), *KERNELS], "build", KERNELS)
                groups = {(report.Owner(row["label"], row["configuration"]), "default") for row in built["expected"]}
                outputs = report.advertised_build_outputs(evidence / "invocation-kernels/events.jsonl", groups)
                if len(outputs) != 3:
                    raise ValueError("Expected three canonical kernel outputs")
                actions = []
                with action_log.open() as stream:
                    for _, action in execution_records(stream):
                        if action is not None and action["targetLabel"] in [label + "_build" for label in KERNELS]:
                            actions.append(action)
                if {action["targetLabel"] for action in actions} != {label + "_build" for label in KERNELS} or len(actions) != 3:
                    raise ValueError("Missing or duplicate kernel build actions")
                for output in outputs:
                    uri = urlsplit(output["uri"])
                    if uri.scheme != "file" or uri.netloc not in ("", "localhost"):
                        raise ValueError("Expected downloaded kernel output")
                    if report.file_record(Path(unquote(uri.path))) != {key: output[key] for key in ("sha256", "bytes")}:
                        raise ValueError("Kernel output does not match native BEP")
                    action, = [action for action in actions if action["targetLabel"] == output["identity"]["label"] + "_build"]
                    if action["runner"] != "remote" or action["cacheHit"] or action["exitCode"] != 0 or action["status"]:
                        raise ValueError("Kernel build was not a successful fresh remote action")
                    if not any(item["path"] == output["name"] and item["digest"]["hash"] == output["sha256"] and int(item["digest"]["sizeBytes"]) == output["bytes"] for item in action["actualOutputs"]):
                        raise ValueError("Kernel action and default output differ")
                report.write_json(evidence / "kernel-builds.json", {"outputs": outputs, "actions": actions})
                invoke("netem-smoke", ["test", "--config=tests-bare", "--test_filter=^TestFirecrackerNetem$",
                       "--test_sharding_strategy=disabled", "--nocache_test_results", "--runs_per_test=1",
                       "--flaky_test_attempts=1", "--test_timeout=300", SMOKE], "test", [SMOKE])
                manifest = report.read_json(report.manifest_path(evidence / "invocation-netem-smoke/events.jsonl"))
                logs = [row for row in manifest["reports"] if row["name"] == "test.log"]
                xmls = [row for row in manifest["reports"] if row["name"] == "test.xml"]
                if len(logs) != 1 or len(xmls) != 1:
                    raise ValueError("Expected one original smoke log and XML")
                phase = evidence / "invocation-netem-smoke"
                log = (phase / logs[0]["path"]).read_text()
                cases = list(ET.parse(phase / xmls[0]["path"]).getroot().iter("testcase"))
                if len(cases) != 1 or cases[0].get("name") != "TestFirecrackerNetem" or any(cases[0].find(tag) is not None for tag in ("failure", "error", "skipped")):
                    raise ValueError("Expected the one positive netem case")
                kernel, = [row for row in outputs if row["identity"]["label"].endswith("x86_64_v6_18")]
                identities = re.findall(r"NETEM_KERNEL sha256=([0-9a-f]{64}) bytes=([0-9]+) path=", log)
                if identities != [(kernel["sha256"], str(kernel["bytes"]))] or "Failed to remove firecracker VM:" in log:
                    raise ValueError("Smoke used a different kernel or failed VM removal")
                if any(marker not in log for marker in ("NETEM_ADDED", "NETEM_CHANGED", "NETEM_CLEANED")):
                    raise ValueError("Guest omitted a required netem operation")
                report.write_json(evidence / "netem-smoke.json", {"kernel": kernel, "case": "TestFirecrackerNetem", "log": logs[0], "xml": xmls[0], "status": "PASS"})
            completed = True
        except (OSError, ValueError, RuntimeError, TimeoutError, subprocess.SubprocessError) as error:
            runner_exit = runner_exit or 1
            report.write_json(evidence / "qualification-error.json", {"type": type(error).__name__, "message": clean(str(error).encode()).decode()})
        finally:
            if staged_kernel:
                retained_kernel.unlink()
            if not completed:
                runner_exit = runner_exit or 1
            # Scan uncompressed evidence before it enters the sole artifact.
            credential_artifacts.verify(auth, evidence)
            def traversal_error(error):
                raise error
            for parent, _, names in os.walk(evidence, onerror=traversal_error):
                for name in names:
                    if any(value in (Path(parent) / name).read_bytes() for value in secrets):
                        raise ValueError("Credential in evidence; publication withheld")
            result = report.bundle(evidence, runner_exit, capture_exit, artifacts / "qualification.tar.gz")
            print(json.dumps({"status": result["status"], "native_exit": result["native_exit"],
                              "artifact": report.file_record(artifacts / "qualification.tar.gz")}), flush=True)
            runner_exit = runner_exit or result["native_exit"] or int(result["status"] != "PASS")
    return runner_exit


if __name__ == "__main__":
    raise SystemExit(main())
