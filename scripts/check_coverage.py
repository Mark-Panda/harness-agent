#!/usr/bin/env python3
import re
import subprocess
import sys


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: check_coverage.py <profile> <minimum-percent>", file=sys.stderr)
        return 2
    profile, minimum = sys.argv[1], float(sys.argv[2])
    output = subprocess.check_output(["go", "tool", "cover", "-func", profile], text=True)
    match = re.search(r"total:\s+\(statements\)\s+([0-9.]+)%", output)
    if not match:
        print(f"coverage total not found for {profile}", file=sys.stderr)
        return 1
    actual = float(match.group(1))
    print(f"{profile}: {actual:.1f}% (minimum {minimum:.1f}%)")
    return 0 if actual >= minimum else 1


if __name__ == "__main__":
    raise SystemExit(main())
