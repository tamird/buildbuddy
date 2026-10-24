"""Read Bazel's concatenated SpawnExec JSON without retaining the whole log."""

from collections.abc import Iterator
import json
from typing import TextIO


def execution_records(stream: TextIO, max_record_chars: int = 64 * 1024 * 1024) -> Iterator[tuple[str, dict | None]]:
    """Yield exact text and objects, including final whitespace for audit hashing.

    The limit applies to one record, not the complete execution log. Invalid or
    oversized records fail closed; exponential reads avoid reparsing a large
    incomplete record once per line. TextIO must preserve newlines and use UTF-8.
    """
    if max_record_chars <= 0:
        raise ValueError("Execution record limit must be positive")
    decoder = json.JSONDecoder()
    pending = ""
    eof = False
    read_size = min(65536, max_record_chars + 1)
    while True:
        offset = len(pending) - len(pending.lstrip())
        if eof and offset == len(pending):
            if pending:
                yield pending, None
            return
        try:
            value, end = decoder.raw_decode(pending, offset)
        except json.JSONDecodeError:
            if eof:
                raise ValueError("Malformed or truncated native SpawnExec JSON") from None
            if len(pending) > max_record_chars:
                raise ValueError("Native SpawnExec record exceeds its character limit") from None
            chunk = stream.read(min(read_size, max_record_chars + 1 - len(pending)))
            eof = not chunk
            pending += chunk
            read_size = min(2 * read_size, max_record_chars + 1)
            continue
        if end > max_record_chars:
            raise ValueError("Native SpawnExec record exceeds its character limit")
        if not isinstance(value, dict):
            raise ValueError("Native SpawnExec record must be an object")
        yield pending[:end], value
        pending = pending[end:]
        read_size = min(65536, max_record_chars + 1)
