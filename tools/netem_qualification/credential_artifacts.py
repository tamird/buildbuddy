# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Keep Actions credentials outside Bazel options and published artifacts."""

import argparse
import json
import os
from pathlib import Path
import stat
import sys


HELPER = """#!/bin/sh
[ "$#" -eq 1 ] && [ "$1" = get ] || exit 2
# Bazel includes helper stderr in errors. Never print file contents on failure.
exec cat -- "$0.json" 2>/dev/null
"""


def prepare(directory: Path) -> None:
    """Create a private file-backed implementation of Bazel's helper protocol."""
    if not directory.is_dir() or stat.S_IMODE(directory.stat().st_mode) != 0o700:
        raise ValueError("Credential directory must have mode 0700")
    key = os.environ["BUILDBUDDY_API_KEY"]
    if not key:
        raise ValueError("Missing BuildBuddy credential")
    payload = json.dumps({"headers": {"x-buildbuddy-api-key": [key]}})
    descriptor = os.open(directory / "helper.json", os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w") as stream:
        stream.write(payload)
    descriptor = os.open(directory / "helper", os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o700)
    with os.fdopen(descriptor, "w") as stream:
        stream.write(HELPER)


def verify(directory: Path, artifacts: Path) -> None:
    """Fail closed before upload if any retained file contains a credential."""
    private = directory.resolve()
    public = artifacts.resolve()
    if private == public or private.is_relative_to(public):
        raise ValueError("Credential directory is inside the artifact tree")
    credentials = json.loads((private / "helper.json").read_text())
    values = [value for entries in credentials["headers"].values() for value in entries]
    if not values or any(not isinstance(value, str) or not value for value in values):
        raise ValueError("Invalid credential inventory")
    # Check raw values and JSON's escaped representation, including chunk edges.
    secrets = {value.encode() for value in values}
    secrets.update(json.dumps(value)[1:-1].encode() for value in values)
    overlap = max(map(len, secrets)) - 1
    if not artifacts.is_dir() or artifacts.is_symlink():
        raise ValueError("Missing or linked artifact directory")
    def traversal_error(error):
        raise error

    paths = (Path(parent) / name
             for parent, directories, files in os.walk(artifacts, onerror=traversal_error, followlinks=False)
             for name in [*directories, *files])
    for path in paths:
        if any(secret in str(path.relative_to(artifacts)).encode() for secret in secrets):
            raise ValueError("Credential in artifact name; upload withheld")
        if path.is_symlink():
            raise ValueError("Artifact tree contains a symlink")
        if path.is_dir():
            continue
        if not path.is_file():
            raise ValueError("Artifact tree contains a non-regular file")
        with path.open("rb") as stream:
            previous = b""
            while chunk := stream.read(1024 * 1024):
                content = previous + chunk
                if any(secret in content for secret in secrets):
                    raise ValueError("Credential detected; artifact upload withheld")
                previous = content[-overlap:] if overlap else b""


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=("prepare", "verify"))
    parser.add_argument("directory", type=Path)
    parser.add_argument("artifacts", type=Path, nargs="?")
    args = parser.parse_args()
    try:
        if args.operation == "prepare":
            prepare(args.directory)
        else:
            if args.artifacts is None:
                raise ValueError("Missing artifact directory")
            verify(args.directory, args.artifacts)
            with Path(os.environ["GITHUB_OUTPUT"]).open("a") as stream:
                stream.write("artifacts_safe=true\n")
    except (KeyError, OSError, TypeError, ValueError):
        # Do not include exception text: parser or OS failures can include input.
        print("Credential preparation or artifact verification failed", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
