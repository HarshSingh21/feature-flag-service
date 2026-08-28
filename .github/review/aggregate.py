#!/usr/bin/env python3
"""Merge per-dimension review findings into one PR comment.

Runs in the aggregate job after every review dimension has uploaded its JSON.
Deliberately dependency-free: ubuntu-latest ships python3, and a review gate that
needs `pip install` is a review gate that breaks on a bad network day.

Two design points worth keeping:

  * A dimension that produced no file is reported as DID NOT REPORT, never folded
    into "clean". A crashed reviewer and a reviewer that found nothing are opposite
    facts, and collapsing them is how a green check starts meaning nothing.
  * Deduplication is by (file, line, category), because several dimensions are
    expected to reach the same defect from different directions. That agreement is
    signal, so it is surfaced rather than hidden.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from collections import OrderedDict

SEVERITY_ORDER = {"blocking": 0, "major": 1, "minor": 2}
SEVERITY_LABEL = {"blocking": "BLOCKING", "major": "MAJOR", "minor": "MINOR"}

# Every dimension the workflow matrix runs. Kept here so a dimension that fails to
# upload is still named in the report rather than vanishing from it.
EXPECTED = [
    "correctness-contracts",
    "concurrency-hotpath",
    "bucketing-compatibility",
    "config-merge-validation",
    "security-observability",
    "api-operability",
]

MARKER = "<!-- claude-parallel-review -->"


def collect(root: str) -> tuple[dict[str, list[dict]], dict[str, str]]:
    """Walk the artifact tree for *.json. Returns (findings by dimension, errors)."""
    found: dict[str, list[dict]] = {}
    errors: dict[str, str] = {}

    for dirpath, _dirnames, filenames in os.walk(root):
        for name in filenames:
            if not name.endswith(".json"):
                continue
            path = os.path.join(dirpath, name)
            slug = name[: -len(".json")]
            try:
                with open(path, encoding="utf-8") as fh:
                    payload = json.load(fh)
            except (OSError, json.JSONDecodeError) as exc:
                errors[slug] = f"unreadable output ({type(exc).__name__}: {exc})"
                continue

            slug = payload.get("dimension") or slug
            raw = payload.get("findings")
            if not isinstance(raw, list):
                errors[slug] = "output had no `findings` array"
                continue
            found[slug] = [f for f in raw if isinstance(f, dict)]

    for slug in EXPECTED:
        if slug not in found and slug not in errors:
            errors[slug] = "did not report — the job failed, timed out, or was skipped"

    return found, errors


def dedupe(by_dim: dict[str, list[dict]]) -> list[dict]:
    """Merge identical findings across dimensions, keeping the highest severity."""
    merged: "OrderedDict[tuple, dict]" = OrderedDict()

    for dim, findings in sorted(by_dim.items()):
        for f in findings:
            sev = str(f.get("severity", "minor")).lower()
            if sev not in SEVERITY_ORDER:
                sev = "minor"
            key = (
                str(f.get("file", "?")),
                str(f.get("line", "?")),
                str(f.get("category", "")).lower(),
            )
            if key in merged:
                entry = merged[key]
                entry["dimensions"].append(dim)

                # Agreement must never soften a finding. Two things can strengthen
                # independently, so they are handled separately:
                #
                #   * A higher severity takes over the whole narrative - summary,
                #     short_summary and failure_scenario - because those sentences
                #     are what justify the severity. Upgrading the label while
                #     keeping the milder dimension's wording produced a BLOCKING row
                #     described in the words of a MINOR one.
                #   * A CONFIRMED verdict wins over PLAUSIBLE at ANY severity. If one
                #     reviewer traced the path end to end, the finding is confirmed,
                #     whatever the other reviewer managed.
                if SEVERITY_ORDER[sev] < SEVERITY_ORDER[entry["severity"]]:
                    entry["severity"] = sev
                    entry["summary"] = f.get("summary") or entry["summary"]
                    entry["short_summary"] = (
                        f.get("short_summary") or f.get("summary") or entry["short_summary"]
                    )
                    entry["failure_scenario"] = (
                        f.get("failure_scenario") or entry["failure_scenario"]
                    )

                if str(f.get("verdict", "")).upper() == "CONFIRMED":
                    entry["verdict"] = "CONFIRMED"
                continue

            merged[key] = {
                "file": key[0],
                "line": key[1],
                "category": f.get("category", "review"),
                "severity": sev,
                "short_summary": f.get("short_summary") or f.get("summary", ""),
                "summary": f.get("summary", ""),
                "failure_scenario": f.get("failure_scenario", ""),
                "verdict": f.get("verdict", "PLAUSIBLE"),
                "dimensions": [dim],
            }

    return sorted(
        merged.values(),
        key=lambda f: (SEVERITY_ORDER[f["severity"]], f["file"], str(f["line"])),
    )


def render(findings: list[dict], errors: dict[str, str], by_dim: dict[str, list[dict]],
           sha: str) -> str:
    counts = {s: 0 for s in SEVERITY_ORDER}
    for f in findings:
        counts[f["severity"]] += 1

    out = [MARKER, "## Parallel code review", ""]

    ran = len(by_dim)
    if findings:
        headline = ", ".join(
            f"**{counts[s]} {SEVERITY_LABEL[s].lower()}**" for s in SEVERITY_ORDER if counts[s]
        )
        out.append(f"{len(findings)} finding(s) across {ran} dimension(s): {headline}.")
    elif ran:
        out.append(f"No findings. {ran} dimension(s) reviewed this diff and reported clean.")
    else:
        out.append("No dimension reported. Treat this as **no review**, not as a pass.")
    out.append("")

    if errors:
        out.append("> [!WARNING]")
        out.append("> Some dimensions did not report, so this review is incomplete:")
        for slug in sorted(errors):
            out.append(f"> - `{slug}` — {errors[slug]}")
        out.append("")

    if findings:
        out.append("| | Where | Finding | Dimension |")
        out.append("|---|---|---|---|")
        for f in findings:
            agree = ""
            if len(f["dimensions"]) > 1:
                agree = f" ×{len(f['dimensions'])}"
            loc = f"`{f['file']}:{f['line']}`"
            summary = str(f["short_summary"]).replace("|", "\\|")
            dims = ", ".join(f"`{d}`" for d in sorted(set(f["dimensions"])))
            out.append(
                f"| **{SEVERITY_LABEL[f['severity']]}**{agree} | {loc} | {summary} | {dims} |"
            )
        out.append("")

        out.append("### Detail")
        out.append("")
        for f in findings:
            verdict = f["verdict"].upper()
            out.append(
                f"**{SEVERITY_LABEL[f['severity']]} · `{f['file']}:{f['line']}` · "
                f"{f['category']} · {verdict}**"
            )
            out.append("")
            if f["summary"]:
                out.append(str(f["summary"]))
                out.append("")
            if f["failure_scenario"]:
                out.append(f"*How it fails:* {f['failure_scenario']}")
                out.append("")
            if len(set(f["dimensions"])) > 1:
                out.append(
                    "*Found independently by "
                    + ", ".join(f"`{d}`" for d in sorted(set(f["dimensions"])))
                    + ".*"
                )
                out.append("")

    out.append("---")
    out.append(
        f"Reviewed `{sha[:7]}` · dimensions are defined in "
        "[`.github/review/`](../tree/HEAD/.github/review) · "
        "`PLAUSIBLE` means the reviewer could not trace the whole path, so check it before acting."
    )
    return "\n".join(out)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--artifacts", required=True, help="root of downloaded artifacts")
    ap.add_argument("--out", required=True, help="markdown output path")
    ap.add_argument("--sha", default="unknown")
    ap.add_argument(
        "--fail-on",
        default="blocking",
        choices=["blocking", "major", "minor", "never"],
        help="lowest severity that should fail the gate",
    )
    args = ap.parse_args()

    by_dim, errors = collect(args.artifacts)
    findings = dedupe(by_dim)
    body = render(findings, errors, by_dim, args.sha)

    with open(args.out, "w", encoding="utf-8") as fh:
        fh.write(body + "\n")

    print(body)

    if args.fail_on == "never":
        return 0
    threshold = SEVERITY_ORDER[args.fail_on]
    gating = [f for f in findings if SEVERITY_ORDER[f["severity"]] <= threshold]
    if gating:
        print(
            f"\n::error::review gate: {len(gating)} finding(s) at or above "
            f"'{args.fail_on}'",
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
