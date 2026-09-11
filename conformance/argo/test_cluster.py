"""Opt-in functional tests against an actual Argo 3.7.16 controller.

Run with MASSIVE_TEST_ARGO_IMAGE, MASSIVE_TEST_ARGO_PLATFORM, KUBECONFIG and
MASSIVE_PYTHON configured. The image must contain the current Massive wheel.
"""

from __future__ import annotations

import hashlib
import json
import os
import secrets
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


def install_workflow(source: str, secret_bindings: dict | None = None) -> None:
    with tempfile.TemporaryDirectory(prefix="massive-argo-") as directory:
        root = Path(directory)
        entry = root / "workflow.py"
        entry.write_text(source)
        bundle = root / "bundle"
        bindings = root / "secret-bindings.json"
        bindings.write_text(json.dumps(secret_bindings or {}))
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
                "--artifact-store",
                "massive-datastore",
                "--artifact-credentials-secret",
                "massive-storage-credentials",
                "--secret-bindings",
                str(bindings),
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


@unittest.skipUnless(
    os.environ.get("MASSIVE_TEST_ARGO_IMAGE"),
    "requires live Argo cluster and runner image",
)
class DecisionConformance(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        kubectl(
            "apply",
            "-f",
            "-",
            document={
                "apiVersion": "v1",
                "kind": "ConfigMap",
                "metadata": {"name": "massive-datastore"},
                "data": {
                    "datastore.json": json.dumps(
                        {
                            "kind": "s3",
                            "bucket": "my-bucket",
                            "region": "us-east-1",
                            "prefix": "massive-conformance",
                            "endpoint": "http://minio:9000",
                            "forcePathStyle": True,
                        }
                    )
                },
            },
        )
        credentials = kubectl("get", "secret", "my-minio-cred", "-o", "json")["data"]
        kubectl(
            "apply",
            "-f",
            "-",
            document={
                "apiVersion": "v1",
                "kind": "Secret",
                "metadata": {"name": "massive-storage-credentials"},
                "data": {
                    "AWS_ACCESS_KEY_ID": credentials["accesskey"],
                    "AWS_SECRET_ACCESS_KEY": credentials["secretkey"],
                },
            },
        )
        source = (Path(__file__).parent / "decisions.py").read_text()
        source = source.replace(
            'IMAGE = "example.invalid/runner@sha256:" + "0" * 64',
            f"IMAGE = {os.environ['MASSIVE_TEST_ARGO_IMAGE']!r}",
        )
        source = source.replace(
            'PLATFORM = "linux/amd64"',
            f"PLATFORM = {os.environ['MASSIVE_TEST_ARGO_PLATFORM']!r}",
        )
        install_workflow(source)
        file_source = (ROOT / "examples/08-artifacts/workflow.py").read_text()
        file_source = file_source.replace(
            '"example.invalid/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"',
            repr(os.environ["MASSIVE_TEST_ARGO_IMAGE"]),
        ).replace(
            'platform="linux/amd64"',
            f"platform={os.environ['MASSIVE_TEST_ARGO_PLATFORM']!r}",
        )
        install_workflow(file_source)
        token = secrets.token_hex(32)
        kubectl(
            "apply",
            "-f",
            "-",
            document={
                "apiVersion": "v1",
                "kind": "Secret",
                "metadata": {"name": "application.credentials"},
                "stringData": {".token": token},
            },
        )
        secret_source = (
            (Path(__file__).parent / "application_secrets.py")
            .read_text()
            .replace(
                'IMAGE = "example.invalid/runner@sha256:" + "0" * 64',
                f"IMAGE = {os.environ['MASSIVE_TEST_ARGO_IMAGE']!r}",
            )
            .replace(
                'PLATFORM = "linux/amd64"',
                f"PLATFORM = {os.environ['MASSIVE_TEST_ARGO_PLATFORM']!r}",
            )
        )
        install_workflow(
            secret_source,
            {"service-token": {"name": "application.credentials", "key": ".token"}},
        )
        cls.runs = {}
        for label, inputs in {
            "positive": {"score": 3},
            "zero": {"score": 0},
            "skip": {"score": -1},
            "empty": {"score": 3, "copies": 0},
            "failure": {"score": 3, "fail": True},
            "files": {"copies": 3},
            "secrets": {
                "digest": hashlib.sha256(token.encode()).hexdigest(),
                "values": [1, 2, 3],
            },
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
                        "workflowTemplateRef": {
                            "name": {
                                "files": "file-artifacts",
                                "secrets": "argo-secrets",
                            }.get(label, "argo-decisions")
                        },
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
        self.assertEqual(
            run["status"]["phase"],
            "Succeeded",
            str(
                [
                    (node["displayName"], node["phase"], node.get("message"))
                    for node in run["status"].get("nodes", {}).values()
                ]
            ),
        )
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

    def test_file_references_are_hydrated_across_pods(self) -> None:
        run = self.completed("files")
        self.assertEqual(
            run["status"]["phase"],
            "Succeeded",
            str(
                [
                    (node["displayName"], node["phase"], node.get("message"))
                    for node in run["status"].get("nodes", {}).values()
                ]
            ),
        )
        root = run["status"]["nodes"][run["metadata"]["name"]]
        result = json.loads(root["outputs"]["parameters"][0]["value"])
        self.assertEqual(
            result,
            {"original": "original", "reports": ["report-0", "report-1", "report-2"]},
        )

    def test_application_secret_reaches_only_declared_item_pods(self) -> None:
        self.successful("secrets", 12)
        run = self.completed("secrets")
        nodes = run["status"]["nodes"]
        authenticated = [
            node
            for node in nodes.values()
            if node.get("type") == "Pod"
            and node.get("templateName") == "map-item-authenticated"
        ]
        self.assertEqual(len(authenticated), 3)
        pods = kubectl(
            "get",
            "pods",
            "-l",
            f"workflows.argoproj.io/workflow={run['metadata']['name']}",
            "-o",
            "json",
        )
        self.assertEqual(
            len(pods["items"]),
            sum(node.get("type") == "Pod" for node in nodes.values()),
        )
        for pod in pods["items"]:
            node = nodes[
                pod["metadata"]["annotations"]["workflows.argoproj.io/node-id"]
            ]
            main = next(
                container
                for container in pod["spec"]["containers"]
                if container["name"] == "main"
            )
            app_variables = [
                variable
                for variable in main.get("env", [])
                if variable["name"] == "APP_TOKEN"
            ]
            self.assertEqual(
                bool(app_variables),
                node.get("templateName") == "map-item-authenticated",
            )
            if app_variables:
                self.assertEqual(
                    app_variables[0]["valueFrom"]["secretKeyRef"],
                    {"name": "application.credentials", "key": ".token"},
                )

    def test_selected_item_failure_cannot_produce_success(self) -> None:
        run = self.completed("failure")
        self.assertEqual(
            run["status"]["phase"],
            "Failed",
            str(
                [
                    (node["displayName"], node["phase"], node.get("message"))
                    for node in run["status"].get("nodes", {}).values()
                ]
            ),
        )
        nodes = run["status"]["nodes"].values()
        self.assertTrue(
            any(
                n["phase"] == "Failed"
                and n.get("type") == "Pod"
                and n.get("templateName") == "map-item-evaluate"
                for n in nodes
            )
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
