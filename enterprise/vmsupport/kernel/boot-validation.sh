#!/bin/bash
set -euo pipefail
expected_source=${QUALIFICATION_COMMIT:?}
credential_rc=${QUALIFICATION_CREDENTIAL_RC:?}
[[ $credential_rc == /* && -r $credential_rc ]]
before_source=42c327ab1bc738e3ac87de5ed4d579bfba11be56
work_deadline=$(python3 -c 'import time; print(time.monotonic() + 30 * 60)')
remaining_work_seconds() {
  python3 - "$work_deadline" <<'DEADLINE'
import math
import sys
import time
remaining = float(sys.argv[1]) - time.monotonic()
if remaining <= 0:
    raise SystemExit("Remote qualification work deadline expired")
print(math.ceil(remaining))
DEADLINE
}
[[ $expected_source =~ ^[0-9a-f]{40}$ ]]
[[ $(git rev-parse HEAD) == "$expected_source" ]]
[[ -z $(git status --porcelain --untracked-files=no) ]]
[[ $(id -u) == 0 && $(uname -m) == x86_64 ]]
provider_bazel=$(type -P bazelisk)
[[ $provider_bazel == /* && -x $provider_bazel ]]
artifacts=${RUNNER_TEMP:?}/qualification
mkdir -p "$artifacts"
out=$(mktemp -d "$artifacts/buildbuddy-kernel-boot.XXXXXX")
raw_directory=$(mktemp -d)
raw_events=$raw_directory/events.jsonl
rc=$artifacts/qualification.bazelrc
finish() {
  local exit_status=$? cleanup_status=0
  local result=${result:-$exit_status}
  if (( result == 0 )); then result=$exit_status; fi
  trap - EXIT
  set +e
  rm -f "$raw_events" || cleanup_status=1
  rmdir "$raw_directory" || cleanup_status=1
  git rev-parse HEAD > "$out/final-head.txt" || cleanup_status=1
  cp MODULE.bazel.lock "$out/final-MODULE.bazel.lock" || cleanup_status=1
  sha256sum MODULE.bazel.lock > "$out/final-lock-sha256.txt" || cleanup_status=1
  git diff --binary --full-index -- MODULE.bazel.lock > "$out/final-lock.patch" || cleanup_status=1
  git status --porcelain --untracked-files=no > "$out/source-after.txt" || cleanup_status=1
  if [[ $(cat "$out/final-head.txt") != "$expected_source" || -s "$out/source-after.txt" ]]; then
    cleanup_status=1
  fi
  printf '%s\n' "$result" > "$out/finish-primary-exit.txt" || cleanup_status=1
  printf '%s\n' "$cleanup_status" > "$out/finish-cleanup-exit.txt" || cleanup_status=1
  if timeout --signal=TERM --kill-after=5s 60s tar -czf "$artifacts/evidence.tar.gz" -C "$out" .; then
    chmod -R u+w "$out" || cleanup_status=1
    rm -rf "$out" || cleanup_status=1
  else
    cleanup_status=1
  fi
  if (( result == 0 )); then result=$cleanup_status; fi
  exit "$result"
}
trap finish EXIT
cat > "$rc" <<'RC'
# Use the repository's existing Linux RBE configuration with a private credential RC.
# Keep the repository's canonical BLAKE3 startup digest.
startup --host_jvm_args=-Xmx4g
common --config=ci-anon
common --config=target-linux-x86
common --config=tests-bare
# Compile remotely; only the selected KVM TestRunner runs on this host.
build --strategy=TestRunner=local
# Match the repository CI lock check; no silent updates or restoration.
common --lockfile_mode=error
build --jobs=400
build --local_resources=cpu=4
build --local_resources=memory=12288
build --noremote_local_fallback
build --remote_timeout=1800
build --remote_retries=0
build --remote_download_outputs=toplevel
build --remote_download_regex=.*[/]test[.](log|xml)$
test --nocache_test_results
test --runs_per_test=1
test --flaky_test_attempts=1
test --test_timeout=300
test --test_sharding_strategy=disabled
test --test_env=PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
common --noannounce_rc
common --color=no
common --curses=no
RC
cp "$rc" "$out/qualification.bazelrc"
git cat-file -p HEAD > "$out/commit.txt"
if ! git cat-file -e "$before_source^{commit}" 2>/dev/null; then
  timeout --signal=TERM --kill-after=5s 60s git fetch --no-tags --depth=1 origin "$before_source"
fi
git cat-file -p "$before_source" > "$out/before-commit.txt"
git diff --binary --full-index "$before_source" "$expected_source" > "$out/source-delta.patch"
git diff --name-only "$before_source" "$expected_source" > "$out/changed-paths.txt"
python3 - "$out/changed-paths.txt" <<'PATHS'
from pathlib import Path
import sys
actual = set(Path(sys.argv[1]).read_text().splitlines())
expected = {'.github/workflows/coverage.yaml', 'enterprise/vmsupport/bin/BUILD', 'enterprise/vmsupport/kernel/boot-validation.sh'}
assert actual == expected, (actual, expected)
PATHS
for file in .bazelrc .bazelversion shared.bazelrc MODULE.bazel MODULE.bazel.lock enterprise/vmsupport/kernel/BUILD enterprise/vmsupport/kernel/defs.bzl enterprise/vmsupport/kernel/rebuild.sh enterprise/vmsupport/kernel/microvm-kernel-x86_64-v5.15.config enterprise/vmsupport/kernel/microvm-kernel-x86_64-v6.1.config enterprise/vmsupport/bin/BUILD enterprise/vmsupport/bundle.go enterprise/vmsupport/BUILD enterprise/server/remote_execution/containers/firecracker/BUILD enterprise/server/remote_execution/containers/firecracker/firecracker_test.go enterprise/server/remote_execution/containers/firecracker/firecracker.go enterprise/server/cmd/goinit/main.go server/testutil/testnetworking/testnetworking.go .github/workflows/coverage.yaml enterprise/vmsupport/kernel/boot-validation.sh; do
  git show "$expected_source:$file" > "$out/${file//\//_}.source"
done
{
  uname -a
  id
  ls -l /dev/kvm /dev/fuse
  findmnt /sys/fs/cgroup
  cat /sys/fs/cgroup/cgroup.controllers
  df -Pk .
  command -v ip iptables mkfs.ext4 bazelisk
} > "$out/host-facts.txt"
[[ -c /dev/kvm && -c /dev/fuse ]]
[[ $(stat -fc %T /sys/fs/cgroup) == cgroup2fs ]]
run_phase() {
  local phase=$1 command=$2 status=0 capture_status=0
  shift 2
  rm -f "$raw_events" || return 125
  printf '%s\n' "$command" "$@" > "$out/$phase-arguments.txt" || return 125
  timeout --signal=INT --kill-after=30s "$(remaining_work_seconds)" "$provider_bazel" --bazelrc="$rc" --bazelrc="$credential_rc" \
    "$command" --build_event_json_file="$raw_events" \
    --execution_log_compact_file="$out/$phase-execution.binpb" "$@" \
    > "$out/$phase-stdout.txt" 2> "$out/$phase-stderr.txt" || status=$?
  printf '%s\n' "$status" > "$out/$phase-exit.txt" || capture_status=1
  timeout --signal=TERM --kill-after=5s 60s python3 - "$raw_events" "$out/$phase-events.jsonl" <<'EVENTS' || capture_status=$?
import hashlib
import json
from pathlib import Path
import shutil
import sys
from urllib.parse import unquote, urlsplit
report_errors = []
report_identities = []
keys = {'id', 'children', 'finished', 'configured', 'completed', 'testResult', 'testSummary', 'aborted', 'namedSetOfFiles'}
source, target = map(Path, sys.argv[1:])
count = 0
with source.open() as stream, target.open('w') as output:
    for line in stream:
        if not line.strip():
            continue
        event = json.loads(line)
        if 'started' in event:
            event = {'id': event['id'], 'started': {'uuid': event['started']['uuid']}}
        elif 'optionsParsed' in event:
            event = {'id': event['id'], 'optionsParsed': {'cmdLine': [arg for arg in event['optionsParsed']['cmdLine'] if arg.startswith(('--jobs=', '--spawn_strategy=', '--strategy=', '--runs_per_test=', '--flaky_test_attempts=', '--test_timeout=', '--test_filter=', '--test_sharding_strategy=', '--test_arg=', '--test_env=', '--platforms=', '--cpu=', '--host_platform=', '--remote_download_outputs=')) or arg in ('--nocache_test_results', '--noremote_local_fallback')]}}
        else:
            event = {key: value for key, value in event.items() if key in keys}
        if 'testResult' in event:
            try:
                label = event['id']['testResult']['label']
                assert label.startswith('//')
                package, target_name = label[2:].split(':')
                reports = {item['name']: item for item in event['testResult']['testActionOutput'] if item['name'] in ('test.xml', 'test.log')}
                assert set(reports) == {'test.xml', 'test.log'}, reports
                for name, report in reports.items():
                    uri = urlsplit(report['uri'])
                    local = Path('bazel-testlogs') / package / target_name / name
                    if uri.scheme == 'file':
                        assert local.resolve() == Path(unquote(uri.path)).resolve(), label
                    else:
                        assert uri.scheme == 'bytestream', report['uri']
                    assert local.stat().st_size <= 64 * 1024 * 1024, local
                    body = local.read_bytes()
                    if uri.scheme == 'bytestream':
                        expected_hash, expected_size = report['uri'].rsplit('/', 2)[-2:]
                        assert uri.path.split('/')[-3] == 'blake3', report['uri']
                        assert len(body) == int(expected_size), label
                        assert len(expected_hash) == 64 and all(c in '0123456789abcdef' for c in expected_hash)
                    # Preserve original local bodies; independently authenticate
                    # any advertised CAS BLAKE3 during final result reduction.
                    report_identities.append({'label': label, 'name': name, 'uri': report['uri'], 'sha256': hashlib.sha256(body).hexdigest(), 'bytes': len(body)})
                    destination = target.parent / target.name[:-len('-events.jsonl')] / package / target_name / name
                    destination.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copyfile(local, destination)
            except (OSError, ValueError, AssertionError) as error:
                report_errors.append({'label': event['id']['testResult']['label'], 'error': str(error)})
        serialized = json.dumps(event)
        assert 'x-buildbuddy-api-key' not in serialized.lower()
        output.write(serialized + '\n')
        count += 1
assert count, 'No complete build events'
target.with_suffix('.local-xml-errors.json').write_text(json.dumps(report_errors) + '\n')
target.with_suffix('.local-xml-identities.json').write_text(json.dumps(report_identities) + '\n')
assert not report_errors, report_errors
EVENTS
  printf '%s\n' "$capture_status" > "$out/$phase-capture-exit.txt" || capture_status=1
  if (( status == 0 )); then status=$capture_status; fi
  return "$status"
}

result=0
run_phase kernels build \
  //enterprise/vmsupport/kernel:guest_kernel_x86_64_v5_15 \
  //enterprise/vmsupport/kernel:guest_kernel_x86_64_v6_1 \
  //enterprise/server/cmd/executor:firecracker \
  //enterprise/server/cmd/executor:jailer \
  //enterprise/server/remote_execution/containers/firecracker:firecracker_test || result=$?
if (( result == 0 )); then
  timeout --signal=TERM --kill-after=5s 60s python3 - "$out" <<'KERNELS' || result=$?
import hashlib
import json
from pathlib import Path
import shutil
import sys
from urllib.parse import unquote, urlsplit
out = Path(sys.argv[1])
expected = {'vmlinux-x86_64-v5.15', 'vmlinux-x86_64-v6.1'}
found = {}
for line in (out / 'kernels-events.jsonl').read_text().splitlines():
    if not line.strip():
        continue
    event = json.loads(line)
    for item in event.get('namedSetOfFiles', {}).get('files', []):
        name = Path(item['name']).name
        if name not in expected:
            continue
        uri = urlsplit(item['uri'])
        local = Path(unquote(uri.path)) if uri.scheme == 'file' else Path('bazel-bin') / item['name']
        assert uri.scheme in ('file', 'bytestream')
        size = local.stat().st_size
        assert 0 < size <= 256 * 1024 * 1024
        body = local.read_bytes()
        assert body[:6] == b'\x7fELF\x02\x01', name
        digest = hashlib.sha256(body).hexdigest()
        if uri.scheme == 'bytestream':
            expected_digest, expected_size = item['uri'].rsplit('/', 2)[-2:]
            assert uri.path.split('/')[-3] == 'blake3', item['uri']
            assert size == int(expected_size)
            assert len(expected_digest) == 64 and all(c in '0123456789abcdef' for c in expected_digest)
            # Final original-artifact reduction verifies the advertised BLAKE3.
        record = {'name': name, 'sha256': digest, 'bytes': size, 'advertised': item}
        if name in found:
            assert found[name] == record
        else:
            shutil.copyfile(local, out / name)
            found[name] = record
assert set(found) == expected, found
(out / 'kernel-artifacts.json').write_text(json.dumps(found, indent=2) + '\n')
KERNELS
fi
if (( result == 0 )); then
  install -m 0755 bazel-bin/enterprise/server/cmd/executor/firecracker /usr/local/bin/firecracker
  install -m 0755 bazel-bin/enterprise/server/cmd/executor/jailer /usr/local/bin/jailer
  sha256sum /usr/local/bin/firecracker /usr/local/bin/jailer > "$out/runtime-binaries.sha256"
fi
for version in v5.15 v6.1; do
  if (( result != 0 )); then break; fi
  enabled=false
  if [[ $version == v6.1 ]]; then enabled=true; fi
  run_phase "$version" test \
    --test_filter='^TestFirecrackerRunSimple$' \
    "--test_arg=--executor.firecracker_enable_linux_6_1=$enabled" \
    //enterprise/server/remote_execution/containers/firecracker:firecracker_test || result=$?
  if (( result == 0 )); then
    python3 - "$out" "$version" <<'CHECK' || result=$?
import json
from pathlib import Path
import sys
import xml.etree.ElementTree as ET
out, version = Path(sys.argv[1]), sys.argv[2]
path = out / version / 'enterprise/server/remote_execution/containers/firecracker/firecracker_test/test.xml'
cases = ET.parse(path).getroot().findall('.//testcase')
assert len(cases) == 1 and cases[0].get('name') == 'TestFirecrackerRunSimple', [case.attrib for case in cases]
assert all(cases[0].find(tag) is None for tag in ('failure', 'error', 'skipped'))
(out / (version + '-census.json')).write_text(json.dumps({'case': cases[0].get('name'), 'passed': True}) + '\n')
CHECK
  fi
done
exit "$result"
