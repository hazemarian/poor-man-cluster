#!/usr/bin/env python3
"""Render every Mermaid diagram in the docs and fail on the ones nobody can read.

A Mermaid block can be wrong in two ways, and only one is visible while reading:

  1. It does not parse. The renderer errors, and a GitHub reader gets a browser
     error box where the diagram should be.
  2. It parses perfectly and is still useless. GitHub fits the diagram to its
     ~830px content column, so a diagram naturally wider than that is scaled
     down, and its labels shrink with it. A seven-node `flowchart LR` measured
     roughly 1900px wide — scale 0.44, about 7px text.

Reading the markup catches neither, so this renders every block and measures it.

The number that matters is **natural width**, not aspect ratio. A tall narrow
diagram (237x998) is never downscaled and reads fine, while a short wide one of
the same aspect does not — so a knee-jerk aspect check flags the harmless one
and misses the real one. This reads the intrinsic size out of the rendered SVG
and computes the scale GitHub will actually apply.

Usage:  python3 scripts/check-mermaid-render.py
Exit:   0 when every diagram renders and stays legible, 1 otherwise.

Requires `mmdc` (npm i -g @mermaid-js/mermaid-cli) on PATH, or set $MMDC.
"""
from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

# GitHub's markdown content column, in CSS pixels.
COLUMN_PX = 830.0

# Mermaid renders label text at roughly 16px. Below the warn threshold the text
# is small enough that a reader notices and zooms; below the hard limit it is
# effectively unreadable in the page.
WARN_SCALE = 0.75
FAIL_SCALE = 0.55

MMDC = os.environ.get("MMDC", "mmdc")


def in_scope() -> list[Path]:
    """The prose that ships to readers. CHANGELOG and the point-in-time test
    reports are records, not claims about the current code."""
    docs = [ROOT / "README.md", ROOT / "agent.md"]
    docs += sorted((ROOT / "docs").glob("*.md"))
    docs += sorted((ROOT / "pmcluster" / "docs").glob("*.md"))
    docs += [ROOT / "pmcluster" / "README.md",
             ROOT / "pmcluster" / "internal" / "ARCHITECTURE.md"]
    return [d for d in docs if d.exists()]


def mermaid_blocks(text: str) -> list[str]:
    blocks, lines, i = [], text.split("\n"), 0
    while i < len(lines):
        if lines[i].strip() == "```mermaid":
            body, i = [], i + 1
            while i < len(lines) and lines[i].strip() != "```":
                body.append(lines[i])
                i += 1
            blocks.append("\n".join(body))
        i += 1
    return blocks


def natural_size(svg: Path) -> tuple[float, float] | None:
    """Intrinsic size. Mermaid writes `width="100%"` with the real dimensions in
    viewBox, so prefer viewBox and fall back to numeric width/height."""
    head = svg.read_text(errors="replace")[:800]
    vb = re.search(r'viewBox="([-\d.\s]+)"', head)
    if vb:
        parts = [p for p in vb.group(1).split() if p]
        if len(parts) == 4:
            w, h = float(parts[2]), float(parts[3])
            if w > 0 and h > 0:
                return w, h
    w = re.search(r'\bwidth="([\d.]+)', head)
    h = re.search(r'\bheight="([\d.]+)', head)
    if w and h:
        return float(w.group(1)), float(h.group(1))
    return None


def main() -> int:
    if shutil.which(MMDC) is None:
        print(f"✗ {MMDC} not found on PATH — install @mermaid-js/mermaid-cli", file=sys.stderr)
        return 1

    failures: list[str] = []
    warnings: list[str] = []
    measured: list[tuple[str, float, float, float]] = []
    unchecked = 0

    tmp = Path(tempfile.mkdtemp(prefix="mermaid-"))
    # Chromium will not start as root in a container without these.
    puppeteer = tmp / "puppeteer.json"
    puppeteer.write_text(json.dumps({"args": ["--no-sandbox", "--disable-setuid-sandbox"]}))

    try:
        for doc in in_scope():
            for n, body in enumerate(mermaid_blocks(doc.read_text()), 1):
                if not body.strip():
                    continue
                label = f"{doc.relative_to(ROOT)} #{n}"
                src, out = tmp / "d.mmd", tmp / "d.svg"
                out.unlink(missing_ok=True)
                src.write_text(body)

                proc = subprocess.run(
                    [MMDC, "-i", str(src), "-o", str(out), "-b", "transparent",
                     "-p", str(puppeteer)],
                    capture_output=True, text=True,
                )
                if proc.returncode != 0 or not out.exists():
                    detail = (proc.stderr or proc.stdout).strip().splitlines()
                    failures.append(
                        f"{label}: did not render — {detail[0] if detail else 'unknown error'}")
                    continue

                size = natural_size(out)
                if size is None:
                    unchecked += 1
                    print(f"  {label:<44} could not read the intrinsic size")
                    continue
                w, h = size
                scale = min(1.0, COLUMN_PX / w)
                text_px = 16.0 * scale
                measured.append((label, w, h, scale))

                if scale < FAIL_SCALE:
                    failures.append(
                        f"{label}: {w:.0f}px wide — GitHub scales it to {scale:.2f}, so labels "
                        f"render at about {text_px:.0f}px. Reflow it: a long `flowchart LR` chain "
                        f"usually wants `flowchart TD`")
                elif scale < WARN_SCALE:
                    warnings.append(
                        f"{label}: {w:.0f}px wide — scaled to {scale:.2f} (~{text_px:.0f}px labels)")

        print()
        for label, w, h, scale in sorted(measured):
            mark = "FAIL" if scale < FAIL_SCALE else ("warn" if scale < WARN_SCALE else "ok")
            print(f"  {label:<44}{w:>6.0f}x{h:<6.0f} natural   scale {scale:>4.2f}   {mark}")
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

    print(f"\nrendered and measured {len(measured)} diagram(s)"
          + (f", {unchecked} unmeasured" if unchecked else ""))
    if not measured:
        print("✗ measured nothing — the extractor or the docs moved", file=sys.stderr)
        return 1

    for w in warnings:
        print(f"  warn  {w}")
    if failures:
        print(f"\n{len(failures)} diagram(s) would be unreadable on GitHub:", file=sys.stderr)
        for f in failures:
            print(f"  ✗ {f}", file=sys.stderr)
        return 1

    print("every diagram renders and stays legible at GitHub's column width")
    return 0


if __name__ == "__main__":
    sys.exit(main())
