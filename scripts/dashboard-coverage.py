#!/usr/bin/env python3
"""Report how many shipped dashboard panels no test asserts anything about.

WHY THIS EXISTS. An "N untested panels remain" figure reached by
subtracting from the figure before it, rather than by measuring, drifts: one
such chain started at a number that did not agree with its own itemized
list (16 claimed, 15 listed), and when it was finally checked, three
different definitions of "untested" gave 15, 6 and 4.

So this prints all three, each with its definition, and refuses to collapse
them into one headline. None of them is "a test asserts a value from this
panel" -- that is not mechanically decidable, because a panel reached through
a helper is named in a variable and a panel named only inside a structural
walk is not really covered. The honest output is a range and its bounds.

Usage:  python3 scripts/dashboard-coverage.py [--list]
"""

import json
import os
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
DASH = ROOT / "deploy" / "grafana" / "dashboards"
TESTS = ROOT / "sink" / "dashboard_sql_test.go"


def panels():
    """Every panel in every shipped dashboard that carries a query."""
    out = []
    for path in sorted(DASH.glob("*.json")):
        dashboard = path.stem
        doc = json.loads(path.read_text())

        def walk(nodes):
            for node in nodes or []:
                walk(node.get("panels"))
                targets = node.get("targets") or []
                sql = any((t.get("rawSql") or "").strip() for t in targets)
                prom = any((t.get("expr") or "").strip() for t in targets)
                if sql or prom:
                    out.append((dashboard, node.get("title"), "SQL" if sql else "PROM"))

        walk(doc.get("panels"))
    return out


def main():
    src = TESTS.read_text()
    # Comments are stripped for the middle measure: a panel discussed in
    # prose is documented, not covered.
    code = "\n".join(re.sub(r"^\s*//.*$", "", line) for line in src.split("\n"))
    literal = set(re.findall(r'panelSQL\(t,\s*"([^"]+)",\s*"([^"]+)"', src))

    rows = panels()
    measures = {
        "never named by a literal panelSQL(dashboard, title) call": [
            r for r in rows if (r[0], r[1]) not in literal and r[2] == "SQL"
        ],
        "title never appears in test CODE (comments stripped)": [
            r for r in rows if f'"{r[1]}"' not in code
        ],
        "title never appears in the test file at all": [
            r for r in rows if f'"{r[1]}"' not in src
        ],
    }

    print(f"{len(rows)} panels carry a query "
          f"({sum(1 for r in rows if r[2] == 'SQL')} SQL, "
          f"{sum(1 for r in rows if r[2] == 'PROM')} Prometheus) "
          f"across {len(set(r[0] for r in rows))} dashboards.\n")
    print("Untested, by three definitions -- none of them authoritative:\n")
    for name, missing in measures.items():
        print(f"  {len(missing):3d}  {name}")
        if "--list" in sys.argv:
            for dashboard, title, kind in missing:
                print(f"         {dashboard}: {title} [{kind}]")
    print("\nThe first over-counts: it misses every panel reached through a")
    print("helper that passes the title in a variable. The last two under-count:")
    print("they accept a structural walk as coverage. Quote the range and the")
    print("definition, never a single decremented integer.")


if __name__ == "__main__":
    main()
