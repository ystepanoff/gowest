#!/usr/bin/env python3
"""Gate CI on the Autobahn|Testsuite conformance report.

`wstest` exits 0 regardless of whether cases passed, so CI cannot rely on its
exit code. This script reads the generated report/index.json and exits non-zero
if any case is non-conformant, printing a per-case summary either way.

A case is conformant when both its "behavior" and "behaviorClose" verdicts are
one of OK / NON-STRICT / INFORMATIONAL / UNIMPLEMENTED. NON-STRICT means the
implementation handled the case acceptably though not in the single strictest
way (commonly close timing); it is not a failure. Anything else (FAILED, WRONG
CODE, UNCLEAN, ...) is a genuine conformance bug and fails the build.
"""

import json
import os
import sys

ACCEPTABLE = {"OK", "NON-STRICT", "INFORMATIONAL", "UNIMPLEMENTED"}

REPORT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "report", "index.json")


def main() -> int:
    if not os.path.exists(REPORT):
        print(f"FAIL: {REPORT} not found — did the Autobahn client run and "
              f"reach the server?", file=sys.stderr)
        return 1

    with open(REPORT) as f:
        index = json.load(f)

    total = 0
    failures = []
    for agent, cases in index.items():
        for case, result in cases.items():
            total += 1
            behavior = result.get("behavior", "MISSING")
            behavior_close = result.get("behaviorClose", "MISSING")
            if behavior not in ACCEPTABLE or behavior_close not in ACCEPTABLE:
                failures.append((agent, case, behavior, behavior_close))

    print(f"Autobahn: {total} cases run, {len(failures)} non-conformant")
    for agent, case, behavior, behavior_close in failures:
        print(f"  FAIL [{agent}] case {case}: "
              f"behavior={behavior} behaviorClose={behavior_close}")

    if total == 0:
        print("FAIL: report contained no cases", file=sys.stderr)
        return 1
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
