#!/usr/bin/env python3
"""Project backend performance logs into fixed numeric events, never raw text."""
import json
import re
import sys

patterns = [
    ("cache_entry_skipped", re.compile(r"prompt state size ([0-9.]+) MiB exceeds cache size limit ([0-9.]+) MiB, skipping"), ("entryMiB", "limitMiB")),
    ("cache_evicted", re.compile(r"(?:cache size limit reached|making room for prompt cache entry), removing oldest entry \(size = ([0-9.]+) MiB\)"), ("entryMiB",)),
    ("prompt_evaluated", re.compile(r"prompt eval time =\s*([0-9.]+) ms /\s*([0-9]+) tokens \(\s*([0-9.]+) ms per token,\s*([0-9.]+) tokens per second\)"), ("durationMs", "tokens", "msPerToken", "tokensPerSecond")),
    ("output_evaluated", re.compile(r"(?<!prompt )eval time =\s*([0-9.]+) ms /\s*([0-9]+) tokens \(\s*([0-9.]+) ms per token,\s*([0-9.]+) tokens per second\)"), ("durationMs", "tokens", "msPerToken", "tokensPerSecond")),
]
for line in json.load(sys.stdin):
    text = line.get("text", "")
    if not isinstance(text, str):
        continue
    text = re.sub(r"\x1b\[[0-9;]*m", "", text)
    for event, pattern, keys in patterns:
        match = pattern.search(text)
        if match:
            result = {"event": event}
            try:
                result.update(zip(keys, (float(value) for value in match.groups())))
            except ValueError:
                continue
            print(json.dumps(result, allow_nan=False))
            break
