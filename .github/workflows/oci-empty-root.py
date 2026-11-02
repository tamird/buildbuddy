"""Keep original native OCI observations before comparing their outcomes."""
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import uuid
import xml.etree.ElementTree as ET

import qualification_report as qr

TARGET = "//enterprise/server/remote_execution/containers/ociruntime:ociruntime_test"
CASES = ("TestRunWithEmptyRootfs/no-layers", "TestRunWithEmptyRootfs/empty-layers")
CONTROLS = ("TestRun", "TestImageStoreExtractsWhiteouts", "TestImageStoreAppliesLayerRootOwnership")
ROOT = Path(os.environ["RUNNER_TEMP"])
PUBLIC = ROOT / "oci-public"
ISSUE = "mount overlayfs: invalid argument"


def now():
    return datetime.now(timezone.utc).isoformat()


def git(*args):
    return subprocess.check_output(["git", *args])


def write(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def diagnostic(lines):
    return [s[:1000] for s in lines if re.search(r'ERROR:|FAILED:|Unexpected error|No space left|permission denied|unavailable|mount overlayfs', s, re.I)][-40:]


def native(arm, source):
    out = ROOT / ("oci-" + arm)
    out.mkdir()
    cmd = ["test", "--config=untrusted-ci", "--test_sharding_strategy=disabled", "--nocache_test_results",
           "--remote_download_outputs=toplevel", "--build_event_json_file=" + str(out / "raw"),
           "--build_event_json_file_path_conversion=false", "--test_output=errors", "--invocation_id=" + str(uuid.uuid4())]
    names = ["TestRunWithEmptyRootfs", *(CONTROLS if arm == "fixed" else ())]
    cmd += ["--test_filter=^(" + "|".join(names) + ")$", TARGET]
    rec = {"arm": arm, "source": source, "owner": TARGET, "arguments": cmd, "started": now(), "verified": False}
    try:
        subprocess.run(["git", "switch", "--detach", source], check=True)
        assert git("rev-parse", "HEAD").decode().strip() == source
        assert not git("status", "--porcelain", "--untracked-files=no").strip()
        (out / "commit_file").write_bytes(git("cat-file", "commit", "HEAD"))
        (out / "source_before").write_bytes(b"")
        (out / "executed_arguments").write_text("\n".join(cmd) + "\n")
        (out / "targets").write_text(TARGET + "\n")
        # Leave the native job enough time to capture the original outputs even on a stalled action.
        with (out / "console").open("w") as f:
            rc = subprocess.run(["timeout", "--signal=TERM", "--kill-after=30s", "35m", "bazelisk", *cmd], stdout=f, stderr=subprocess.STDOUT).returncode
        rec.update(native_exit=rc, execution_finished=now(), diagnostics=diagnostic((out / "console").read_text(errors="replace").splitlines()))
        (out / "source_after").write_bytes(git("status", "--porcelain", "--untracked-files=no"))
        (out / "final_head").write_bytes(git("rev-parse", "HEAD"))
        (out / "native_exit").write_text(str(rc))
        events = out / "events"
        captured = qr.capture(out / "raw", events, Path("bazel-out"), out / "reports")
        (out / "capture_exit").write_text("1" if captured["errors"] else "0")
        fields = ("commit_file", "source_before", "source_after", "final_head", "executed_arguments", "native_exit", "capture_exit")
        context = dict(zip(fields, fields))
        context.update(expected_source=source, command_kind="test", execution_finished_at=rec["execution_finished"])
        qr.write_json(out / "context", context)
        v = qr.verdict(events, qr.manifest_path(events), out / "context", out / "targets", ())
        rec["original"] = {k: v[k] for k in ("status", "missing", "errors", "case_counts", "executed", "failures", "expected")}
        reports = {r["name"]: out / r["path"] for r in captured["reports"]}
        for key, ext in (("test.log", "log"), ("test.xml", "xml")):
            if key in reports:
                shutil.copyfile(reports[key], PUBLIC / (arm + "." + ext))
        raw = [json.loads(line) for line in (out / "raw").read_text().splitlines()]
        results = [e for e in raw if "testResult" in e and e.get("id", {}).get("testResult", {}).get("label") == TARGET]
        (result,) = results
        r = result["testResult"]
        rec["test_result"] = r
        assert not (r.get("cachedLocally") or r.get("executionInfo", {}).get("cachedRemotely")) and r.get("executionInfo", {}).get("strategy") == "remote"
        assert len(v["executed"]) == 1 and not v["missing"] and not v["errors"] and not captured["errors"]
        doc = ET.parse(reports["test.xml"])
        log = reports["test.log"].read_text(errors="replace")
        cases = {}
        for node in doc.iter("testcase"):
            name = node.get("name", "")
            fail = node.find("failure")
            evidence = "" if fail is None else ET.tostring(fail, encoding="unicode")
            # rules_go may leave the diagnostic in the original log rather than in the XML node.
            block = re.search(r"(?m)^\s*--- FAIL: " + re.escape(name) + r" \([^\n]*\)\n(.*?)(?=^\s*--- (?:FAIL|PASS|SKIP):|^=== RUN|\Z)", log, re.S | re.M)
            cases[name] = {"failed": fail is not None or node.find("error") is not None,
                           "skipped": node.find("skipped") is not None or node.get("status") == "notrun",
                           "matched_mount_error": ISSUE in evidence or bool(block and ISSUE in block.group(1))}
        rec["cases"] = cases
        if arm == "stock":
            rec["verified"] = (rc == 3 and v["status"] == "FAIL" and r["status"] == "FAILED"
                               and all(cases[c] == {"failed": True, "skipped": False, "matched_mount_error": True} for c in CASES))
        else:
            rec["verified"] = (rc == 0 and v["status"] == "PASS" and r["status"] == "PASSED"
                               and all(cases[c] == {"failed": False, "skipped": False, "matched_mount_error": False} for c in (*CASES, *CONTROLS)))
    except Exception as e:
        rec["record_error"] = f"{type(e).__name__}: {e}"
    rec["recorded_at"] = now()
    write(PUBLIC / (arm + ".json"), rec)
    print(arm, rec.get("native_exit"), rec.get("original", {}).get("status"), rec["verified"], rec.get("record_error", ""), flush=True)
    return rec


stock = native("stock", os.environ["CONTROL"])
fixed = native("fixed", os.environ["PRODUCT"])
ok = stock["verified"] and fixed["verified"]
write(PUBLIC / "verdict.json", {"verified": ok, "base": os.environ["BASE"], "stock": stock["source"], "product": fixed["source"],
      "status": "PASS" if ok else "INCOMPLETE", "control_verified": stock["verified"], "fixed_verified": fixed["verified"], "at": now()})
sys.exit(not ok)
