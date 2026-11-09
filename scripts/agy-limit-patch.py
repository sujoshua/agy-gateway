#!/usr/bin/env python3
"""Raises the harness's request-time context limit from 256000 to 983040 tokens.

agy-context-unlock removes the harness's input truncation, but the harness
still caps every request at checkpoint_config.max_token_limit, which it
defaults to 256000 when the runtime sends no compaction_config (the ACP
runtime never does). This script rewrites that one constant in place.

Usage, as AGY_GW_HARNESS_PATCH_CMD (the gateway appends the harness path):

    python3 agy-limit-patch.py [--dsh /path/to/agy-patch.py] HARNESS

With --dsh, agy-context-unlock's agy-patch.py runs on HARNESS first. The
constant is located by its surrounding instructions; if they are not found
exactly once the file is left alone and the script fails, so a different
runtime version is never patched blindly. Matched against runtime 1.4.5.
"""

import argparse
import subprocess
import sys

# Each entry: (architecture, official bytes, patched bytes). The first
# instruction sets the compaction threshold (140000) and is left unchanged;
# the rest load max_token_limit.
PATCHES = [
    (
        "x86_64",
        # mov ebx, 0x222e0; mov esi, 0x3e800
        bytes.fromhex("bbe0220200" "be00e80300"),
        # mov ebx, 0x222e0; mov esi, 0xf0000
        bytes.fromhex("bbe0220200" "be00000f00"),
    ),
    (
        "arm64",
        # mov x3, #0x22e0; movk x3, #0x2, lsl #16;
        # mov x4, #0xe800; movk x4, #0x3, lsl #16
        bytes.fromhex("035c84d2" "4300a0f2" "04009dd2" "6400a0f2"),
        # same threshold; mov x4, #0; movk x4, #0xf, lsl #16
        bytes.fromhex("035c84d2" "4300a0f2" "040080d2" "e401a0f2"),
    ),
]


def main():
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("--dsh", metavar="AGY_PATCH_PY", help="run agy-context-unlock's agy-patch.py on the harness first")
    p.add_argument("harness")
    args = p.parse_args()

    if args.dsh:
        cmd = [sys.executable, args.dsh, "--in-place", "--no-backup", "--binary", args.harness]
        if subprocess.run(cmd, check=False).returncode != 0:
            sys.exit("agy-limit-patch: agy-patch.py failed")

    with open(args.harness, "rb") as f:
        data = f.read()
    for arch, old, new in PATCHES:
        if data.count(new) == 1 and data.count(old) == 0:
            print(f"agy-limit-patch: {arch} limit already raised")
            return
        if data.count(old) != 1:
            continue
        at = data.find(old)
        with open(args.harness, "r+b") as f:
            f.seek(at)
            f.write(new)
        print(f"agy-limit-patch: {arch} max_token_limit 256000 -> 983040 at offset {at:#x}")
        return
    sys.exit("agy-limit-patch: the limit was not found exactly once; is this runtime 1.4.5?")


if __name__ == "__main__":
    main()
