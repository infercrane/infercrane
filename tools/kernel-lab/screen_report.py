#!/usr/bin/env python3
"""Turn a CPU screen result into a readable verdict and an exit code."""

import json
import sys

STAGES = ("smoke", "shape_sweep", "numerical_stability", "determinism", "edge_cases")


def main() -> int:
    result = json.load(sys.stdin)
    correctness = result["correctness"]
    for stage in STAGES:
        print("  %-20s %s" % (stage, "pass" if correctness[stage] else "FAIL"))
    print("  %-20s %s" % ("sanitizer, timing", "not run (need a GPU)"))
    if all(correctness[stage] for stage in STAGES):
        print("PASS: correct on CPU. GPU checks are still required before use.")
        return 0
    print("FAIL at %s" % result.get("failed_stage", "correctness"))
    return 1


if __name__ == "__main__":
    sys.exit(main())
