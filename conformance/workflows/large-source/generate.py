"""Write the deterministic resource tree for the large-source workflow.

The tree models an application package: thousands of small prompt and rule
files plus a few bundled directories of several hundred KiB each, together
well above Argo's 700 KiB embedded transport limit. It is generated rather
than committed so the repository stays small.
"""

from __future__ import annotations

import hashlib
import sys
from pathlib import Path


def _text(label: str, size: int) -> bytes:
    lines: list[str] = []
    counter = 0
    while sum(len(line) for line in lines) < size:
        digest = hashlib.sha256(f"{label}:{counter}".encode()).hexdigest()
        lines.append(f"- {label} {counter}: {digest}\n")
        counter += 1
    return "".join(lines).encode()[:size]


def generate(workflow_root: Path) -> None:
    resources = workflow_root / "resources"
    for index in range(1500):
        path = resources / "prompts" / f"group-{index % 12:02d}" / f"prompt-{index:04d}.md"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(_text(f"prompt {index}", 160 + index % 200))
    for pack in range(6):
        for index in range(150):
            path = (
                resources / "rules" / f"pack-{pack}" / f"family-{index % 5}" / f"rule-{index:03d}.yaml"
            )
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(_text(f"rule {pack}/{index}", 400))
    for bundle in range(3):
        for index in range(8):
            path = resources / "bundles" / f"bundle-{bundle}" / "data" / f"part-{index}.txt"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(_text(f"bundle {bundle}/{index}", 64 * 1024))


if __name__ == "__main__":
    generate(Path(sys.argv[1]))
