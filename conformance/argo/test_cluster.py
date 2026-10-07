"""Opt-in functional tests against an actual Argo 3.7.16 controller.

Run with MASSIVE_TEST_ARGO_IMAGE, MASSIVE_TEST_ARGO_PLATFORM, KUBECONFIG and
MASSIVE_PYTHON configured. The image must contain the current Massive wheel.
"""

from __future__ import annotations

import base64
import hashlib
import json
import os
import secrets
import socket
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
DATASTORE = {
    "kind": "s3",
    "bucket": "my-bucket",
    "region": "us-east-1",
    "prefix": "massive-conformance",
    "endpoint": "http://minio:9000",
    "forcePathStyle": True,
}


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


def install_workflow(
    source: str,
    secret_bindings: dict | None = None,
    *,
    selector: str = "",
    build_args: tuple[str, ...] = (),
    pyproject: str | None = None,
    lock: str | None = None,
    publish: bool = False,
) -> None:
    with tempfile.TemporaryDirectory(prefix="massive-argo-") as directory:
        root = Path(directory)
        entry = root / "workflow.py"
        entry.write_text(source)
        if pyproject is not None:
            (root / "pyproject.toml").write_text(pyproject)
        if lock is not None:
            (root / "uv.lock").write_text(lock)
        bundle = root / "bundle"
        bindings = root / "secret-bindings.json"
        bindings.write_text(json.dumps(secret_bindings or {}))
        subprocess.run(
            [
                "go",
                "run",
                "./cmd/massive",
                "build",
                str(entry) + selector,
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
                *build_args,
            ],
            cwd=ROOT,
            check=True,
            timeout=120,
        )
        if publish:
            publish_bundle(bundle)
        kubectl(
            "apply",
            "-f",
            str(bundle / "runtime-configmap.json"),
            "-f",
            str(bundle / "workflow-template.json"),
        )


def massive(*args: str, environment: dict[str, str] | None = None) -> str:
    return subprocess.run(
        ["go", "run", "./cmd/massive", *args],
        cwd=ROOT,
        check=True,
        capture_output=True,
        env=None if environment is None else {**os.environ, **environment},
        text=True,
        timeout=300,
    ).stdout


def publish_bundle(bundle: Path) -> None:
    """Upload an object-store-v0 bundle's source archives to the cluster's MinIO
    through a port-forward, as `massive publish` does for a real deployment."""
    root = bundle.parent
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        port = probe.getsockname()[1]
    forward = subprocess.Popen(
        [
            os.environ.get("KUBECTL", "kubectl"),
            "-n",
            "argo",
            "port-forward",
            "svc/minio",
            f"{port}:9000",
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    try:
        assert forward.stdout is not None
        forward.stdout.readline()  # "Forwarding from 127.0.0.1:<port> -> 9000"
        descriptor = root / "datastore.json"
        descriptor.write_text(
            json.dumps({**DATASTORE, "endpoint": f"http://127.0.0.1:{port}"})
        )
        credentials = kubectl("get", "secret", "my-minio-cred", "-o", "json")["data"]
        published = json.loads(
            massive(
                "publish",
                str(bundle),
                "--datastore-config",
                str(descriptor),
                "--json",
                environment={
                    "AWS_ACCESS_KEY_ID": base64.b64decode(
                        credentials["accesskey"]
                    ).decode(),
                    "AWS_SECRET_ACCESS_KEY": base64.b64decode(
                        credentials["secretkey"]
                    ).decode(),
                },
            )
        )
        assert [archive["created"] for archive in published["sourceArchives"]] == [
            True
        ], published
    finally:
        forward.terminate()
        forward.wait(timeout=30)


def fixture_source(path: Path) -> str:
    return (
        path.read_text()
        .replace(
            'IMAGE = "example.invalid/runner@sha256:" + "0" * 64',
            f"IMAGE = {os.environ['MASSIVE_TEST_ARGO_IMAGE']!r}",
        )
        .replace(
            'PLATFORM = "linux/amd64"',
            f"PLATFORM = {os.environ['MASSIVE_TEST_ARGO_PLATFORM']!r}",
        )
    )


def run_locally(source: str, inputs: dict) -> dict:
    """Run a workflow with the local target, the expected result on Argo."""
    with tempfile.TemporaryDirectory(prefix="massive-argo-local-") as directory:
        entry = Path(directory) / "workflow.py"
        entry.write_text(source)
        return json.loads(
            massive(
                "run",
                str(entry),
                "--input",
                json.dumps(inputs),
                "--store",
                str(Path(directory) / "store"),
                "--project",
                "massive/argo-local",
                "--json",
            )
        )["result"]


def install_large_source() -> dict:
    """Build the >1 MiB source fixture for object-store-v0, publish its archive
    through a port-forward to the cluster's MinIO, and return its local result."""
    fixture = ROOT / "conformance/workflows/large-source"
    with tempfile.TemporaryDirectory(prefix="massive-argo-large-") as directory:
        root = Path(directory)
        workflow = root / "workflow"
        workflow.mkdir()
        (workflow / "pyproject.toml").write_text(
            (fixture / "pyproject.toml").read_text()
        )
        (workflow / "workflow.py").write_text(
            (fixture / "workflow.py")
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
        subprocess.run(
            [sys.executable, str(fixture / "generate.py"), str(workflow)],
            check=True,
            timeout=60,
        )
        local = json.loads(
            massive(
                "run",
                str(workflow / "workflow.py"),
                "--input",
                "{}",
                "--store",
                str(root / "store"),
                "--project",
                "massive/large-source",
                "--json",
            )
        )
        build = [
            "build",
            str(workflow / "workflow.py"),
            "--namespace",
            "argo",
            "--service-account",
            "default",
            "--artifact-store",
            "massive-datastore",
            "--artifact-credentials-secret",
            "massive-storage-credentials",
        ]
        embedded = subprocess.run(
            ["go", "run", "./cmd/massive", *build, "--output", str(root / "embedded")],
            cwd=ROOT,
            capture_output=True,
            text=True,
            timeout=300,
            check=False,
        )
        assert (
            embedded.returncode != 0
            and "--runtime-transport object-store-v0" in embedded.stderr
        ), embedded.stderr
        bundle = root / "bundle"
        massive(
            *build, "--output", str(bundle), "--runtime-transport", "object-store-v0"
        )
        configmap = json.loads((bundle / "runtime-configmap.json").read_text())
        assert sorted(configmap["binaryData"]) == ["massive-plan.json"], configmap[
            "binaryData"
        ].keys()
        publish_bundle(bundle)
        kubectl(
            "apply",
            "-f",
            str(bundle / "runtime-configmap.json"),
            "-f",
            str(bundle / "workflow-template.json"),
        )
        return local["result"]


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
                "data": {"datastore.json": json.dumps(DATASTORE)},
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
        retry_source = (
            (Path(__file__).parent / "retries.py")
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
        install_workflow(retry_source)
        # Never published: a changed source identity whose archive is absent
        # from the store is refused by the pod's preflight (exit 68), never retried.
        install_workflow(
            retry_source + "\n# unpublished object-store variant\n",
            build_args=(
                "--name",
                "argo-unpublished",
                "--runtime-transport",
                "object-store-v0",
            ),
        )
        composition_source = (
            (ROOT / "packages/python/tests/fixtures/composed_workflow.py")
            .read_text()
            .replace(
                '"example.invalid/python@sha256:" + "0" * 64',
                repr(os.environ["MASSIVE_TEST_ARGO_IMAGE"]),
            )
            .replace(
                'platform="linux/amd64"',
                f"platform={os.environ['MASSIVE_TEST_ARGO_PLATFORM']!r}",
            )
        )
        install_workflow(composition_source, selector="#graph")
        cls.large_source_result = install_large_source()
        # The build host lacks nothing here; pods check the image itself.
        preflight_source = (
            (Path(__file__).parent / "preflight.py")
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
        locked = Path(__file__).parent / "locked"
        install_workflow(
            (locked / "workflow.py")
            .read_text()
            .replace(
                'IMAGE = "example.invalid/runner@sha256:" + "0" * 64',
                f"IMAGE = {os.environ['MASSIVE_TEST_ARGO_IMAGE']!r}",
            )
            .replace(
                'PLATFORM = "linux/amd64"',
                f"PLATFORM = {os.environ['MASSIVE_TEST_ARGO_PLATFORM']!r}",
            ),
            pyproject=(locked / "pyproject.toml").read_text(),
            lock=(locked / "uv.lock").read_text(),
        )
        # The published variant proves pods check the fetched object-store
        # archive: an empty extraction would pass as an undeclared project.
        for name, dependency, published in (
            ("argo-preflight", "pydantic>=2", False),
            ("argo-preflight-missing", "absent-package-for-preflight>=1", False),
            ("argo-preflight-published", "absent-package-for-preflight>=1", True),
        ):
            install_workflow(
                preflight_source.replace('NAME = "argo-preflight"', f"NAME = {name!r}"),
                pyproject=(
                    "[project]\n"
                    f'name = "{name}"\n'
                    'version = "0.1.0"\n'
                    f'dependencies = ["massive-workflows", "{dependency}"]\n'
                ),
                build_args=("--runtime-transport", "object-store-v0")
                if published
                else (),
                publish=published,
            )
        large_values = fixture_source(Path(__file__).parent / "large_values.py")
        install_workflow(large_values)
        cls.local_values = {
            label: run_locally(large_values, inputs)
            for label, inputs in {
                "large-values": {"count": 20000},
                "small-values": {"count": 10},
            }.items()
        }
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
            "retry": {"permanent": False},
            "nonretryable": {"permanent": True},
            "composed-approved": {"value": 5},
            "composed-rejected": {"value": -1},
            "large-source": {},
            "unpublished": {"permanent": False},
            "preflight": {"value": 21},
            "locked": {"value": 21},
            "preflight-missing": {"value": 21},
            "preflight-published": {"value": 21},
            "large-values": {"count": 20000},
            "small-values": {"count": 10},
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
                                "retry": "argo-retries",
                                "nonretryable": "argo-retries",
                                "composed-approved": "composed",
                                "composed-rejected": "composed",
                                "large-source": "large-source",
                                "unpublished": "argo-unpublished",
                                "preflight": "argo-preflight",
                                "locked": "argo-locked",
                                "preflight-missing": "argo-preflight-missing",
                                "preflight-published": "argo-preflight-published",
                                "large-values": "large-values",
                                "small-values": "large-values",
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

    def test_reused_child_graph_inside_selected_branch(self) -> None:
        for label, expected in (("composed-approved", 7), ("composed-rejected", 0)):
            run = self.completed(label)
            self.assertEqual(run["status"]["phase"], "Succeeded")
            root = run["status"]["nodes"][run["metadata"]["name"]]
            self.assertEqual(
                json.loads(root["outputs"]["parameters"][0]["value"]),
                {"value": expected},
            )

    def test_object_store_source_matches_local_execution(self) -> None:
        run = self.completed("large-source")
        self.assertEqual(
            run["status"]["phase"], "Succeeded", str(run["status"].get("message"))
        )
        root = run["status"]["nodes"][run["metadata"]["name"]]
        result = json.loads(root["outputs"]["parameters"][0]["value"])
        self.assertEqual(result, self.large_source_result)
        self.assertGreater(result["files"], 2000)
        self.assertGreater(result["bytes"], 1024 * 1024)
        self.assertEqual(len(self.pods(run, "map-item-inventory")), 3)

    def test_multi_mb_values_cross_every_boundary_by_reference(self) -> None:
        for label in ("large-values", "small-values"):
            run = self.completed(label)
            self.assertEqual(
                run["status"]["phase"], "Succeeded", str(run["status"].get("message"))
            )
            root = run["status"]["nodes"][run["metadata"]["name"]]
            result = json.loads(root["outputs"]["parameters"][0]["value"])
            self.assertEqual(result, self.local_values[label])
        self.assertEqual(self.local_values["large-values"]["annotated"], 20000)
        run = self.completed("large-values")
        outputs = {
            node["displayName"]: node["outputs"]["parameters"][0]["value"]
            for node in run["status"]["nodes"].values()
            if node.get("type") == "Pod" and node.get("outputs", {}).get("parameters")
        }
        # Every multi-MB value is a reference in Argo; the summary stays inline.
        for task in (
            "generate",
            "route",
            "split",
            "collect",
            "flatten",
            "route-select",
        ):
            self.assertTrue(
                outputs[task].startswith('@{"hash":"sha256:'), outputs[task][:80]
            )
        self.assertTrue(outputs["summarize"].startswith("{"), outputs["summarize"])
        items = [
            json.loads(value)
            for name, value in outputs.items()
            if name.startswith("invoke(")
        ]
        self.assertEqual(len(items), 4)
        self.assertTrue(all("ref" in item for item in items), items)

    def test_missing_published_source_is_not_retried(self) -> None:
        run = self.completed("unpublished")
        self.assertEqual(run["status"]["phase"], "Failed")
        attempts = self.pods(run, "step-flaky")
        self.assertEqual(len(attempts), 1, attempts)
        self.assertEqual(attempts[0]["phase"], "Failed")
        self.assertIn("exit code 68", attempts[0].get("message", ""), attempts[0])
        self.assertIn("massive publish", self.main_log(run))

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

    def pods(self, run: dict, template: str) -> list[dict]:
        return [
            node
            for node in run["status"]["nodes"].values()
            if node.get("type") == "Pod" and node.get("templateName") == template
        ]

    def test_retryable_failures_publish_new_attempts(self) -> None:
        # Items resolve from attempt 2: (1 * 2) + (2 * 2).
        self.successful("retry", 6)
        run = self.completed("retry")
        for template, attempts in {"step-flaky": 2, "map-item-guarded": 4}.items():
            phases = sorted(node["phase"] for node in self.pods(run, template))
            self.assertEqual(
                phases, ["Failed"] * (attempts // 2) + ["Succeeded"] * (attempts // 2)
            )

    def test_non_retryable_failure_is_not_retried(self) -> None:
        run = self.completed("nonretryable")
        self.assertEqual(run["status"]["phase"], "Failed")
        items = self.pods(run, "map-item-guarded")
        self.assertEqual([node["phase"] for node in items], ["Failed", "Failed"])

    def test_pod_preflight_accepts_a_satisfied_project(self) -> None:
        self.successful("preflight", 42)

    def test_pod_preflight_checks_a_packaged_locked_project(self) -> None:
        # A [build-system] project with default groups: pods check the locked
        # runtime set without installing the project or its groups.
        run = self.completed("locked")
        self.assertEqual(run["status"]["phase"], "Succeeded", str(run.get("status")))
        root = run["status"]["nodes"][run["metadata"]["name"]]
        self.assertEqual(
            json.loads(root["outputs"]["parameters"][0]["value"]), "value  42"
        )
        self.assertEqual(
            [node["phase"] for node in self.pods(run, "step-render")], ["Succeeded"]
        )

    def main_log(self, run: dict) -> str:
        """The runtime's own diagnostics from the workflow's pods."""
        return subprocess.run(
            [
                os.environ.get("KUBECTL", "kubectl"),
                "-n",
                "argo",
                "logs",
                "-l",
                f"workflows.argoproj.io/workflow={run['metadata']['name']}",
                "-c",
                "main",
            ],
            check=True,
            capture_output=True,
            text=True,
            timeout=60,
        ).stdout

    def test_pod_preflight_reads_the_published_object_store_archive(self) -> None:
        run = self.completed("preflight-published")
        self.assertEqual(run["status"]["phase"], "Failed")
        pods = self.pods(run, "step-double")
        self.assertEqual([node["phase"] for node in pods], ["Failed"])
        self.assertIn("exit code 68", pods[0].get("message", ""))
        self.assertIn("MISSING_REQUIREMENT", self.main_log(run))

    def test_pod_preflight_failure_is_not_retried(self) -> None:
        # retry(3) would schedule three pods; exit 68 is non-retryable, and
        # the image's missing dependency stops the step before author code.
        run = self.completed("preflight-missing")
        self.assertEqual(run["status"]["phase"], "Failed")
        pods = self.pods(run, "step-double")
        self.assertEqual([node["phase"] for node in pods], ["Failed"])
        self.assertIn("exit code 68", pods[0].get("message", ""))

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
