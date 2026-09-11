"""Opt-in functional tests against an actual Argo 3.7.16 controller.

Run with MASSIVE_TEST_ARGO_IMAGE, MASSIVE_TEST_ARGO_PLATFORM, KUBECONFIG and
MASSIVE_PYTHON configured. The image must contain the current Massive wheel.
"""

from __future__ import annotations

import json
import os
import subprocess
import tempfile
import time
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


def kubectl(*args: str, document: dict | None = None) -> dict:
    command = [os.environ.get("KUBECTL", "kubectl"), "-n", "argo", *args]
    result = subprocess.run(
        command,
        input=json.dumps(document) if document else None,
        check=True,
        capture_output=True,
        text=True,
        timeout=60,
    )
    return json.loads(result.stdout) if result.stdout.strip().startswith("{") else {}


@unittest.skipUnless(
    os.environ.get("MASSIVE_TEST_ARGO_IMAGE"),
    "requires live Argo cluster and runner image",
)
class DecisionConformance(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.directory = tempfile.TemporaryDirectory(prefix="massive-argo-")
        cls.addClassCleanup(cls.directory.cleanup)
        root = Path(cls.directory.name)
        source = (Path(__file__).parent / "decisions.py").read_text()
        source = source.replace(
            'IMAGE = "example.invalid/runner@sha256:" + "0" * 64',
            f"IMAGE = {os.environ['MASSIVE_TEST_ARGO_IMAGE']!r}",
        )
        source = source.replace(
            'PLATFORM = "linux/amd64"',
            f"PLATFORM = {os.environ['MASSIVE_TEST_ARGO_PLATFORM']!r}",
        )
        entry = root / "workflow.py"
        entry.write_text(source)
        bundle = root / "bundle"
        subprocess.run(
            [
                "go",
                "run",
                "./cmd/massive",
                "build",
                str(entry),
                "--output",
                str(bundle),
                "--namespace",
                "argo",
                "--service-account",
                "default",
            ],
            cwd=ROOT,
            check=True,
            timeout=120,
        )
        kubectl(
            "apply",
            "-f",
            str(bundle / "runtime-configmap.json"),
            "-f",
            str(bundle / "workflow-template.json"),
        )
        cls.runs = {}
        for label, inputs in {
            "positive": {"score": 3},
            "zero": {"score": 0},
            "skip": {"score": -1},
            "empty": {"score": 3, "copies": 0},
            "failure": {"score": 3, "fail": True},
        }.items():
            run = kubectl(
                "create",
                "-f",
                "-",
                "-o",
                "json",
                document={
                    "apiVersion": "argoproj.io/v1alpha1",
                    "kind": "Workflow",
                    "metadata": {"generateName": f"massive-{label}-"},
                    "spec": {
                        "workflowTemplateRef": {"name": "argo-decisions"},
                        "arguments": {
                            "parameters": [
                                {"name": "input", "value": json.dumps(inputs)}
                            ]
                        },
                    },
                },
            )
            cls.runs[label] = run["metadata"]["name"]

    def completed(self, label: str) -> dict:
        name = self.runs[label]
        deadline = time.monotonic() + 240
        run = {}
        while time.monotonic() < deadline:
            run = kubectl("get", "workflow", name, "-o", "json")
            if run.get("status", {}).get("phase") in {"Succeeded", "Failed", "Error"}:
                return run
            time.sleep(2)
        self.fail(
            f"Workflow {name} did not finish: {json.dumps(run.get('status', {}))}"
        )

    def successful(self, label: str, expected: int) -> dict:
        run = self.completed(label)
        self.assertEqual(run["status"]["phase"], "Succeeded", json.dumps(run["status"]))
        root = run["status"]["nodes"][run["metadata"]["name"]]
        self.assertEqual(
            json.loads(root["outputs"]["parameters"][0]["value"]), expected
        )
        return {
            node["displayName"]: node
            for node in run["status"]["nodes"].values()
            if node.get("boundaryID") == run["metadata"]["name"]
        }

    def test_nested_selected_branch_and_map(self) -> None:
        nodes = self.successful("positive", 6)
        self.assertEqual(nodes["zero"]["phase"], "Skipped")
        self.assertEqual(nodes["skip"]["phase"], "Skipped")

    def test_empty_map_inside_selected_branch(self) -> None:
        self.successful("empty", 0)

    def test_inactive_nested_decision_never_reads_missing_outputs(self) -> None:
        nodes = self.successful("skip", -1)
        self.assertEqual(nodes["inner"]["phase"], "Omitted")
        self.assertEqual(nodes["inner-select"]["phase"], "Omitted")

    def test_inactive_map_does_not_block_select(self) -> None:
        nodes = self.successful("zero", 0)
        self.assertEqual(nodes["evaluate"]["phase"], "Omitted")

    def test_selected_item_failure_cannot_produce_success(self) -> None:
        run = self.completed("failure")
        self.assertEqual(run["status"]["phase"], "Failed", json.dumps(run["status"]))
        nodes = run["status"]["nodes"].values()
        self.assertTrue(
            any(n["phase"] == "Failed" and n.get("type") == "Pod" for n in nodes)
        )
        self.assertFalse(
            any(
                n["displayName"] == "outer-select" and n["phase"] == "Succeeded"
                for n in nodes
            )
        )


if __name__ == "__main__":
    for required in (
        "MASSIVE_TEST_ARGO_IMAGE",
        "MASSIVE_TEST_ARGO_PLATFORM",
        "KUBECONFIG",
    ):
        if not os.environ.get(required):
            raise SystemExit(
                f"{required} is required; run scripts/test-argo.sh to provision conformance"
            )
    unittest.main(verbosity=2)
